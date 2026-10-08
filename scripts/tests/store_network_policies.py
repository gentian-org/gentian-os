#!/usr/bin/env python3
# =============================================================================
# scripts/tests/store_network_policies.py — the assertions behind
# test-store-network-policies.sh.
#
# Renders, with helm and no cluster, what a cluster would run: the data-plane
# ApplicationSet (for the parameters Argo CD passes each engine's chart), each
# engine's chart (for its NetworkPolicy) and the engine itself from the pinned
# package the Cluster composition installs (for the pods the policy has to
# select and the ports they serve).
#
#   store_network_policies.py shape <store>     the policy against the server
#   store_network_policies.py clients <store>   the policy against the clients
#   store_network_policies.py wiring            what the policies lean on
#   store_network_policies.py off               the switch
#
# <store> is the system function: postgresql, mariadb, cache, s3.
# =============================================================================
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
CLIENTS = ROOT / "scripts/tests/store-clients.yaml"
COMPOSITION = ROOT / "crossplane/compositions/cluster-default.yaml"
CNPG_APPLICATION = ROOT / "kernel/bootstrap/chart/templates/cnpg.yaml"
NAME_LABEL = "kubernetes.io/metadata.name"
TIER, FUNCTION = "gentianos.io/tier", "gentianos.io/function"

STORES = ("postgresql", "mariadb", "cache", "s3")

# The ports a server's pods declare that no policy lists, and why. A port a
# pod declares has to be in a rule or here: a third state is a port somebody
# forgot, and on a cluster that is a probe or a client that hangs.
CLOSED = {
    "postgresql": {9187: "metrics: nothing the platform runs scrapes it"},
    "s3": {9001: "the console: nothing routes to it and nothing uses it"},
}

# CloudNativePG builds the instance pods, not a chart in this repository, so
# what it puts on them cannot be rendered here. Pinned from its source at the
# version the cnpg Application installs (chart cloudnative-pg 0.23.0 is
# CloudNativePG 1.25.0): pkg/specs/pods.go for the labels and the ports,
# pkg/specs/jobs.go for the Jobs. `wiring` fails when the Application moves to
# another chart version, which is the moment to read those two files again.
CNPG_CHART_VERSION = "0.23.0"
CNPG_INSTANCE_PORTS = {"postgresql": 5432, "metrics": 9187, "status": 8000}
CNPG_OPERATOR_LABELS = {"app.kubernetes.io/name": "cloudnative-pg", "app.kubernetes.io/instance": "cnpg"}


def cnpg_instance_labels(cluster):
    return {"cnpg.io/cluster": cluster, "cnpg.io/instanceName": cluster + "-1",
            "cnpg.io/instanceRole": "primary", "cnpg.io/podRole": "instance"}


def cnpg_job_labels(cluster):
    return {"cnpg.io/cluster": cluster, "cnpg.io/instanceName": cluster + "-1", "cnpg.io/jobRole": "initdb"}


class Failure(Exception):
    pass


def helm(*args):
    run = subprocess.run(["helm", "template", *args], cwd=ROOT, capture_output=True, text=True)
    if run.returncode != 0:
        raise Failure(f"helm template {' '.join(args)}: {run.stderr.strip()}")
    return [d for d in yaml.safe_load_all(run.stdout) if isinstance(d, dict)]


def layout():
    """Every namespace of kernel/namespaces.yaml with the labels it carries."""
    doc = yaml.safe_load((ROOT / "kernel/namespaces.yaml").read_text())
    out = {}
    for ns in doc["kernel"]:
        out[ns["name"]] = {TIER: "kernel", FUNCTION: ns["function"]}
    for ns in doc["system"]:
        tier = "system-dmz" if ns["function"].endswith("-dmz") else "system"
        out[ns["name"]] = {TIER: tier, FUNCTION: ns["function"]}
    for labels_of, name in ((out[n], n) for n in out):
        labels_of[NAME_LABEL] = name
    return out


def namespace_of(entry, namespaces):
    """A client's namespace as (name, labels): by name from the layout, or
    written out for a namespace the layout does not hold."""
    ns = entry["namespace"]
    if isinstance(ns, str):
        if ns not in namespaces:
            raise Failure(f"{entry['client']}: {ns} is not a namespace of kernel/namespaces.yaml")
        return ns, namespaces[ns]
    return ns["name"], {**ns.get("labels", {}), NAME_LABEL: ns["name"]}


def appsets(*extra):
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        f.write("namespaces:\n")
        for line in (ROOT / "kernel/namespaces.yaml").read_text().splitlines():
            f.write("  " + line + "\n")
        f.flush()
        return helm("appsets", "kernel/appsets", "-f", f.name, *extra)


def data_plane(*extra):
    """What the data-plane ApplicationSet deploys, per system function: the
    chart's path, its namespace and the Helm parameters Argo CD passes it."""
    sets = [d for d in appsets(*extra) if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == "gentian-data-plane"]
    if len(sets) != 1:
        raise Failure("no gentian-data-plane ApplicationSet rendered")
    spec = sets[0]["spec"]
    stage, elements = None, None
    for generator in spec["generators"][0]["matrix"]["generators"]:
        items = generator["list"]["elements"]
        if "env" in items[0]:
            stage = items[0]["env"]
        else:
            elements = items
    params = spec["template"]["spec"]["source"]["helm"]["parameters"]
    namespaces = layout()
    out = {}
    for element in elements:
        fn = namespaces.get(element["namespace"], {}).get(FUNCTION)
        if fn not in STORES:
            raise Failure(f"the data plane deploys {element['app']} into {element['namespace']}, which is not a store's namespace")
        resolved = {}
        for p in params:
            resolved[p["name"]] = p["value"].replace("{{.namespace}}", element["namespace"]).replace("{{.env}}", stage)
        out[fn] = {"path": element["path"], "namespace": element["namespace"], "params": resolved}
    if set(out) != set(STORES):
        raise Failure(f"the data plane deploys {sorted(out)}, want {sorted(STORES)}")
    return out


def render_store(app):
    """One engine's chart, as Argo CD renders it."""
    args = ["store", app["path"], "-n", app["namespace"]]
    for name, value in app["params"].items():
        args += ["--set", f"{name}={value}"]
    return helm(*args)


def policies_of(docs):
    return [d for d in docs if d.get("kind") == "NetworkPolicy"]


def the_policy(store, apps):
    found = policies_of(render_store(apps[store]))
    if len(found) != 1:
        raise Failure(f"{len(found)} NetworkPolicies rendered for {store}, want exactly one")
    return found[0]


def engine(store, apps):
    """The server as the cluster runs it: its pods' labels and ports, the
    other pods its installation starts, and any policy it brings itself."""
    docs = render_store(apps[store])
    if store == "postgresql":
        clusters = [d for d in docs if d.get("kind") == "Cluster"]
        if len(clusters) != 1:
            raise Failure("the tenant-postgres chart does not render exactly one Cluster")
        name = clusters[0]["metadata"]["name"]
        return {
            "servers": [{"name": name, "labels": cnpg_instance_labels(name), "ports": CNPG_INSTANCE_PORTS}],
            "others": [{"name": name + "-1-initdb", "labels": cnpg_job_labels(name)}],
            "policies": [],
        }

    # The release the Cluster composition composes: which package, at which
    # version, with which ConfigMaps of the engine's chart as its values.
    entry = re.search(r'\(dict\s+"fn"\s+"%s"\s+"chart"\s+"([a-z]+)"\s+"version"\s+"([0-9.]+)"\s+"values"\s+\(list ([^)]*)\)' % store,
                      COMPOSITION.read_text())
    if not entry:
        raise Failure(f"the Cluster composition composes no release for {store}")
    chart, version, maps = entry.group(1), entry.group(2), re.findall(r'"([^"]+)"', entry.group(3))
    package = ROOT / f"charts/infra/packages/{chart}-{version}.tgz"
    if not package.exists():
        raise Failure(f"{package.relative_to(ROOT)} is not in the repository")
    args = [chart, str(package), "-n", apps[store]["namespace"]]
    held = []
    for name in maps:
        found = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == name]
        if len(found) != 1:
            raise Failure(f"the {store} chart renders no ConfigMap {name} for the release to read")
        f = tempfile.NamedTemporaryFile("w", suffix=".yaml")
        f.write(found[0]["data"]["values.yaml"])
        f.flush()
        held.append(f)
        args += ["-f", f.name]
    rendered = helm(*args)

    servers, others = [], []
    services = [d for d in rendered if d.get("kind") == "Service"]
    for d in rendered:
        if d.get("kind") not in ("Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"):
            continue
        template = d["spec"]["jobTemplate"]["spec"]["template"] if d["kind"] == "CronJob" else d["spec"]["template"]
        labels = template["metadata"].get("labels") or {}
        ports = {}
        for c in template["spec"].get("initContainers", []) + template["spec"]["containers"]:
            for p in c.get("ports") or []:
                if p.get("protocol", "TCP") != "TCP":
                    raise Failure(f"{d['metadata']['name']} declares a port that is not TCP: {p}")
                ports[p.get("name", str(p["containerPort"]))] = p["containerPort"]
        pod = {"name": d["metadata"]["name"], "labels": labels, "ports": ports}
        # A server is a pod a Service of the release sends connections to.
        behind = [s for s in services if matches(s["spec"].get("selector") or {"<none>": ""}, labels)]
        (servers if behind and d["kind"] not in ("Job", "CronJob") else others).append(pod)
    # A Service that renames a port would make the policy's number the wrong
    # one for half the readers: Calico matches the pod's port.
    for s in services:
        for p in s["spec"]["ports"]:
            target = p.get("targetPort", p["port"])
            for server in servers:
                number = server["ports"].get(target, target)
                if number != p["port"]:
                    raise Failure(f"Service {s['metadata']['name']} port {p['port']} reaches the pod on {number}")
    return {"servers": servers, "others": others, "policies": policies_of(rendered)}


def matches(selector, labels):
    return all(labels.get(k) == v for k, v in selector.items())


def match_labels(selector, where):
    """A selector's matchLabels. Anything else a selector can say is refused:
    this file would have to evaluate it, and no rule here needs it."""
    if set(selector) - {"matchLabels"}:
        raise Failure(f"{where} selects by {sorted(selector)}; only matchLabels is understood here")
    return selector.get("matchLabels") or {}


def rules(policy):
    """Each ingress rule as (ports, peers), with the mistakes that widen a
    rule refused: no ports (every port), no `from` (everybody)."""
    out = []
    for rule in policy["spec"].get("ingress") or []:
        ports = rule.get("ports") or []
        if not ports:
            raise Failure("a rule with no ports admits every port")
        if any(p.get("protocol") != "TCP" or not isinstance(p.get("port"), int) for p in ports):
            raise Failure("every port must be a number, named with its protocol")
        if not rule.get("from"):
            raise Failure(f"ports {[p['port'] for p in ports]} are listed with no source, which admits everybody")
        for peer in rule["from"]:
            if not peer or set(peer) - {"namespaceSelector", "podSelector"}:
                raise Failure(f"a peer names {sorted(peer)}; a client is a namespace selector, a pod selector or both")
        out.append(({p["port"] for p in ports}, rule["from"]))
    return out


def admitted(policy, ns_name, ns_labels, pod_labels, port):
    """Whether the policy lets this pod open this port, by the semantics of a
    NetworkPolicy: a peer with no namespace selector means the policy's own
    namespace, with no pod selector every pod of the namespaces it names."""
    for ports, peers in rules(policy):
        if port not in ports:
            continue
        for peer in peers:
            if "namespaceSelector" in peer:
                if not matches(match_labels(peer["namespaceSelector"], "a peer's namespace"), ns_labels):
                    continue
            elif ns_name != policy["metadata"]["namespace"]:
                continue
            if "podSelector" in peer and not matches(match_labels(peer["podSelector"], "a peer's pods"), pod_labels):
                continue
            return True
    return False


def check_shape(store):
    apps = data_plane()
    policy = the_policy(store, apps)
    eng = engine(store, apps)
    if policy["metadata"].get("namespace") != apps[store]["namespace"]:
        raise Failure(f"the policy is in {policy['metadata'].get('namespace')}, the server in {apps[store]['namespace']}")
    if policy["spec"].get("policyTypes") != ["Ingress"]:
        raise Failure("the policy must restrict ingress and say nothing about egress")
    if eng["policies"]:
        raise Failure(f"the engine's own chart renders NetworkPolicies too ({[p['metadata']['name'] for p in eng['policies']]}); "
                      "policies add up, so what it admits is admitted")
    selector = match_labels(policy["spec"]["podSelector"], "the policy")
    if not selector:
        raise Failure("the policy selects every pod of the namespace")
    if not eng["servers"]:
        raise Failure("the engine renders no server pod")
    for pod in eng["servers"]:
        if not matches(selector, pod["labels"]):
            raise Failure(f"the policy's selector {selector} does not match the server {pod['name']}, labelled {pod['labels']}")
    for pod in eng["others"]:
        if matches(selector, pod["labels"]):
            raise Failure(f"the policy also selects {pod['name']}, which is not the server")

    served = set()
    for pod in eng["servers"]:
        served |= set(pod["ports"].values())
    listed = set().union(*(ports for ports, _ in rules(policy)))
    closed = set(CLOSED.get(store, {}))
    if listed & closed:
        raise Failure(f"ports {sorted(listed & closed)} are both listed and recorded as closed")
    if served - listed - closed:
        raise Failure(f"the server declares ports {sorted(served - listed - closed)} that are neither listed nor recorded as closed")
    if listed - served:
        raise Failure(f"the policy lists ports {sorted(listed - served)} the server does not declare")
    if closed - served:
        raise Failure(f"ports {sorted(closed - served)} are recorded as closed and the server does not declare them")

    # A namespace a rule names is one the layout has: a name nothing carries
    # admits nobody, and the client it was written for hangs.
    name_peers = {
        match_labels(peer["namespaceSelector"], "a peer's namespace").get(NAME_LABEL)
        for _, peers in rules(policy) for peer in peers if "namespaceSelector" in peer
    } - {None}
    unknown = name_peers - set(layout())
    if unknown:
        raise Failure(f"the policy names namespaces {sorted(unknown)} that kernel/namespaces.yaml does not have")


def check_clients(store):
    apps = data_plane()
    policy = the_policy(store, apps)
    namespaces = layout()
    table = yaml.safe_load(CLIENTS.read_text())
    wanted = [c for c in table["clients"] if c["store"] == store]
    refused = [c for c in table["denied"] if c["store"] == store]
    if not wanted or not refused:
        raise Failure(f"{CLIENTS.name} lists no client, or nothing to refuse, for {store}")
    problems = []
    for c in wanted:
        name, labels = namespace_of(c, namespaces)
        if not admitted(policy, name, labels, c.get("podLabels") or {}, c["port"]):
            problems.append(f"NOT ADMITTED: {c['client']} ({name}, port {c['port']})")
    for c in refused:
        name, labels = namespace_of(c, namespaces)
        if admitted(policy, name, labels, c.get("podLabels") or {}, c["port"]):
            problems.append(f"ADMITTED: {c['client']} ({name}, port {c['port']})")
    # Every source a rule names is somebody's: a peer no client of the table
    # comes through is a door left open for nobody.
    for ports, peers in rules(policy):
        for peer in peers:
            one = {"metadata": policy["metadata"], "spec": {"ingress": [{"from": [peer], "ports": [{"protocol": "TCP", "port": p} for p in ports]}]}}
            used = False
            for c in wanted:
                name, labels = namespace_of(c, namespaces)
                used = used or admitted(one, name, labels, c.get("podLabels") or {}, c["port"])
            if not used:
                problems.append(f"a rule admits {peer} on {sorted(ports)} and {CLIENTS.name} names no client that comes that way")
    if problems:
        raise Failure("\n        ".join(problems))


def check_wiring():
    namespaces = layout()
    apps = data_plane()
    table = yaml.safe_load(CLIENTS.read_text())

    # The parameters the ApplicationSet passes are the layout's names.
    for store, app in apps.items():
        params = app["params"]
        if params.get("networkPolicy.enabled") != "true":
            raise Failure(f"{store}: the ApplicationSet passes networkPolicy.enabled={params.get('networkPolicy.enabled')!r} by default")
        for key, fn in (("control", "control"), ("data", "data"), ("postgresql", "postgresql"),
                        ("mariadb", "mariadb"), ("authentication", "authentication")):
            value = params.get(f"networkPolicy.namespaces.{key}")
            if namespaces.get(value, {}).get(FUNCTION) != fn:
                raise Failure(f"{store}: networkPolicy.namespaces.{key} is {value!r}, which is not the {fn} namespace")
        if namespaces[app["namespace"]][FUNCTION] != store:
            raise Failure(f"{store} is deployed into {app['namespace']}")
        # And a chart rendered by hand, with its own defaults, is the same policy.
        by_hand = policies_of(helm("store", app["path"], "-n", app["namespace"], "--set", f"namespace={app['namespace']}"))
        if [p["spec"] for p in by_hand] != [the_policy(store, apps)["spec"]]:
            raise Failure(f"{store}: the chart's own defaults render a different policy from the one the ApplicationSet's parameters do")

    # The operator's pods carry the labels the table says, where it says.
    operators = []
    for d in helm("gentian-os", "charts/gentian-os", "-n", "kernel-control"):
        if d.get("kind") != "Deployment":
            continue
        pod = d["spec"]["template"]
        if [c.get("command") for c in pod["spec"]["containers"]] == [["/manager"]]:
            operators.append((d["metadata"].get("namespace"), pod["metadata"].get("labels") or {}))
    if len(operators) != 1:
        raise Failure("the operator chart does not render exactly one operator Deployment")
    for c in table["clients"]:
        if "operator" in (c.get("podLabels") or {}).values() and (c["namespace"], c["podLabels"]) != operators[0]:
            raise Failure(f"{c['client']}: the table says {c['namespace']} {c['podLabels']}, the chart renders {operators[0]}")
    if namespaces.get(operators[0][0], {}).get(FUNCTION) != "control":
        raise Failure(f"the operator is rendered into {operators[0][0]}")

    # A tenant namespace carries the label the rules admit, whichever kind of
    # tenant it is; the composition is the only thing that makes one.
    fixtures = sorted((ROOT / "crossplane/tests/unit/render").glob("tenant-*/expected.yaml"))
    seen = 0
    for path in fixtures:
        for d in yaml.safe_load_all(path.read_text()):
            manifest = ((d or {}).get("spec") or {}).get("forProvider", {}).get("manifest") or {}
            if manifest.get("kind") != "Namespace":
                continue
            seen += 1
            tier = (manifest["metadata"].get("labels") or {}).get(TIER)
            if tier != "tenant":
                raise Failure(f"{path.parent.name}: namespace {manifest['metadata']['name']} is composed with tier {tier!r}")
    if seen < 3:
        raise Failure(f"only {seen} composed tenant namespaces found in the render fixtures")
    if table["tenantNamespace"]["labels"].get(TIER) != "tenant":
        raise Failure("the table's tenant namespace is not labelled as a tenant's")

    # CloudNativePG: the version the pinned labels and ports were read at, and
    # the namespace its operator runs in.
    application = CNPG_APPLICATION.read_text()
    if f'targetRevision: "{CNPG_CHART_VERSION}"' not in application:
        raise Failure(f"the cnpg Application no longer installs chart {CNPG_CHART_VERSION}: read CloudNativePG's pod spec at the "
                      "new version, then move the pin in this file")
    if 'namespace: {{ include "ns" (list . "data") }}' not in application:
        raise Failure("the cnpg Application no longer deploys into the data namespace")
    for c in table["clients"]:
        if c["client"].startswith("CloudNativePG's operator"):
            if c["namespace"] != "kernel-data" or c["podLabels"] != CNPG_OPERATOR_LABELS or c["port"] != CNPG_INSTANCE_PORTS["status"]:
                raise Failure("the table's CloudNativePG operator is not the one pinned here")


def check_off():
    # The bootstrap chart hands the switch to the ApplicationSets...
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        f.write("namespaces:\n")
        for line in (ROOT / "kernel/namespaces.yaml").read_text().splitlines():
            f.write("  " + line + "\n")
        f.flush()
        base = ["boot", "kernel/bootstrap/chart", "-f", f.name, "-f", "kernel/platforms.yaml",
                "--set-string", "appsets.enabled=true", "--set-string", "kernelDomain=k.example", "--set-string", "cluster=c",
                "--set-string", "versions.headlamp.chart=0.0.0", "--set-string", "versions.headlamp.repo=https://example.invalid"]
        for passed, want in ((None, "true"), ("true", "true"), ("false", "false")):
            args = base + (["--set-string", f"storeNetworkPolicies={passed}"] if passed else [])
            root = [d for d in helm(*args) if d.get("kind") == "Application" and d["metadata"]["name"] == "gentian-appsets"]
            if len(root) != 1:
                raise Failure("the bootstrap chart renders no gentian-appsets Application")
            got = root[0]["spec"]["source"]["helm"]["valuesObject"].get("storeNetworkPolicies")
            if got != want:
                raise Failure(f"STORE_NETWORK_POLICIES={passed}: the ApplicationSets are handed storeNetworkPolicies={got!r}, want {want!r}")
    # ...which hand it to every engine's chart, as a word or as a boolean...
    for extra in (["--set-string", "storeNetworkPolicies=false"], ["--set", "storeNetworkPolicies=false"]):
        apps = data_plane(*extra)
        for store in STORES:
            if apps[store]["params"].get("networkPolicy.enabled") != "false":
                raise Failure(f"{store}: switched off with {extra[0]}, the ApplicationSet still passes "
                              f"networkPolicy.enabled={apps[store]['params'].get('networkPolicy.enabled')!r}")
            # ...and no chart renders a policy.
            left = policies_of(render_store(apps[store]))
            if left:
                raise Failure(f"{store}: a NetworkPolicy is rendered with the switch off")
    # On, every chart renders its one.
    apps = data_plane()
    for store in STORES:
        the_policy(store, apps)


def main():
    args = sys.argv[1:]
    try:
        if args[:1] == ["shape"] and args[1:] and args[1] in STORES:
            check_shape(args[1])
        elif args[:1] == ["clients"] and args[1:] and args[1] in STORES:
            check_clients(args[1])
        elif args == ["wiring"]:
            check_wiring()
        elif args == ["off"]:
            check_off()
        else:
            print(f"usage: {sys.argv[0]} shape|clients <{'|'.join(STORES)}> | wiring | off", file=sys.stderr)
            return 2
    except Failure as e:
        print(f"        {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
