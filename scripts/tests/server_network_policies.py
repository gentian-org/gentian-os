#!/usr/bin/env python3
# =============================================================================
# scripts/tests/server_network_policies.py — the NetworkPolicies on the
# servers that are not shared stores: the kernel's own PostgreSQL. Run by
# test-store-network-policies.sh beside the stores'.
#
# Rendered with helm and no cluster, from what a cluster would run: the
# bootstrap chart's kernel-postgres Application (for the parameters Argo CD
# passes), the server's chart (for its policy), and the clients from the
# `servers` section of scripts/tests/store-clients.yaml.
#
#   server_network_policies.py shape <server>     the policy against the server
#   server_network_policies.py clients <server>   the policy against the clients
#   server_network_policies.py wiring             what the policies lean on
#   server_network_policies.py off                the switch
#   server_network_policies.py modes              what a value may change
#
# <server> is kernel-postgres.
#
# A rule with ports and no `from` admits everybody. It is refused here unless
# every port of it is recorded in OPEN with the reason, and nothing is: a
# server with a port that faces the internet would be the one to add it.
# =============================================================================
import sys
import tempfile

import yaml

from store_network_policies import (
    CLIENTS, CNPG_APPLICATION, CNPG_CHART_VERSION, CNPG_INSTANCE_PORTS, CNPG_OPERATOR_LABELS, FUNCTION, NAME_LABEL,
    ROOT, Failure, appsets, cnpg_instance_labels, cnpg_job_labels, helm, layout, match_labels, matches,
    namespace_of, policies_of,
)

SERVERS = ("kernel-postgres",)

# Ports a server's pods serve that its policy lists nowhere, and why.
CLOSED = {
    "kernel-postgres": {9187: "metrics: nothing the platform runs scrapes it"},
}

# Ports admitted from any source, and why no source can be named. A port is
# here or behind a `from`; a rule may not be open by accident.
OPEN = {}

BOOTSTRAP = ["boot", "kernel/bootstrap/chart", "-f", "kernel/platforms.yaml",
             "--set-string", "appsets.enabled=true", "--set-string", "kernelDomain=k.example", "--set-string", "cluster=c",
             "--set-string", "versions.headlamp.chart=0.0.0", "--set-string", "versions.headlamp.repo=https://example.invalid"]


def layout_values():
    f = tempfile.NamedTemporaryFile("w", suffix=".yaml")
    f.write("namespaces:\n")
    for line in (ROOT / "kernel/namespaces.yaml").read_text().splitlines():
        f.write("  " + line + "\n")
    f.flush()
    return f


def bootstrap_application(name, *extra):
    """One Application of the bootstrap chart, as B-01 renders it."""
    with layout_values() as f:
        found = [d for d in helm(*BOOTSTRAP, "-f", f.name, *extra)
                 if d.get("kind") == "Application" and d["metadata"]["name"] == name]
    if len(found) != 1:
        raise Failure(f"the bootstrap chart renders no {name} Application")
    return found[0]


def application_set(name, *extra):
    found = [d for d in appsets(*extra) if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == name]
    return found[0] if len(found) == 1 else None


def deployed(*extra):
    """What Argo CD deploys for each server: the chart's path, the namespace
    and the Helm parameters."""
    out = {}
    app = bootstrap_application("kernel-postgres", *extra)
    source = app["spec"]["source"]
    out["kernel-postgres"] = {
        "path": source["path"], "namespace": app["spec"]["destination"]["namespace"],
        "params": {p["name"]: p["value"] for p in source["helm"]["parameters"]},
        "prune": app["spec"]["syncPolicy"]["automated"]["prune"],
    }
    return out


def render(app, *extra):
    args = ["server", app["path"], "-n", app["namespace"]]
    for name, value in app["params"].items():
        args += ["--set", f"{name}={value}"]
    return helm(*args, *extra)


def the_policy(server, apps, *extra):
    found = policies_of(render(apps[server], *extra))
    if len(found) != 1:
        raise Failure(f"{len(found)} NetworkPolicies rendered for {server}, want exactly one")
    return found[0]


def engine(server, apps, *extra):
    """The server's pods (labels and ports) and the other pods beside it."""
    docs = render(apps[server], *extra)
    if server == "kernel-postgres":
        clusters = [d for d in docs if d.get("kind") == "Cluster"]
        if len(clusters) != 1:
            raise Failure("the kernel-postgres chart does not render exactly one Cluster")
        name = clusters[0]["metadata"]["name"]
        return {
            "servers": [{"name": name, "labels": cnpg_instance_labels(name), "ports": CNPG_INSTANCE_PORTS}],
            "others": [{"name": name + "-1-initdb", "labels": cnpg_job_labels(name)},
                       {"name": "CloudNativePG's operator", "labels": CNPG_OPERATOR_LABELS}],
        }
    raise Failure(f"no engine is known for {server}")


def rules(policy, server):
    """Each ingress rule as (ports, peers); peers is None for a rule that
    names no source, which only the ports recorded in OPEN may have."""
    out = []
    for rule in policy["spec"].get("ingress") or []:
        ports = rule.get("ports") or []
        if not ports:
            raise Failure("a rule with no ports admits every port")
        if any(p.get("protocol") != "TCP" or not isinstance(p.get("port"), int) for p in ports):
            raise Failure("every port must be a number, named with its protocol")
        numbers = {p["port"] for p in ports}
        if "from" not in rule:
            stray = numbers - set(OPEN.get(server, {}))
            if stray:
                raise Failure(f"ports {sorted(stray)} are listed with no source, which admits everybody, and are not recorded as open")
            out.append((numbers, None))
            continue
        if not rule["from"]:
            raise Failure(f"ports {sorted(numbers)} have an empty `from`, which admits everybody")
        if numbers & set(OPEN.get(server, {})):
            raise Failure(f"ports {sorted(numbers & set(OPEN[server]))} are recorded as open and listed behind a source")
        for peer in rule["from"]:
            if not peer or set(peer) - {"namespaceSelector", "podSelector"}:
                raise Failure(f"a peer names {sorted(peer)}; a client is a namespace selector, a pod selector or both")
        out.append((numbers, rule["from"]))
    return out


def peer_admits(policy, peer, ns_name, ns_labels, pod_labels):
    if "namespaceSelector" in peer:
        if not matches(match_labels(peer["namespaceSelector"], "a peer's namespace"), ns_labels):
            return False
    elif ns_name != policy["metadata"]["namespace"]:
        return False
    return "podSelector" not in peer or matches(match_labels(peer["podSelector"], "a peer's pods"), pod_labels)


def admitted(policy, server, client, namespaces, only=None):
    """Whether the policy lets this client open its port. A connection from
    outside the cluster is admitted by a rule without a source and by no
    other; `only` narrows the question to one rule's one peer."""
    for ports, peers in rules(policy, server):
        if client["port"] not in ports:
            continue
        if peers is None:
            if only is None or only == (frozenset(ports), None):
                return True
            continue
        if client.get("external"):
            continue
        name, labels = namespace_of(client, namespaces)
        for i, peer in enumerate(peers):
            if only is not None and only != (frozenset(ports), i):
                continue
            if peer_admits(policy, peer, name, labels, client.get("podLabels") or {}):
                return True
    return False


def table(server):
    doc = yaml.safe_load(CLIENTS.read_text())["servers"]
    return [c for c in doc["clients"] if c["server"] == server], [c for c in doc["denied"] if c["server"] == server]


def check_shape(server, apps=None, *extra):
    apps = apps or deployed()
    policy = the_policy(server, apps, *extra)
    eng = engine(server, apps, *extra)
    if policy["metadata"].get("namespace") != apps[server]["namespace"]:
        raise Failure(f"the policy is in {policy['metadata'].get('namespace')}, the server in {apps[server]['namespace']}")
    if policy["spec"].get("policyTypes") != ["Ingress"]:
        raise Failure("the policy must restrict ingress and say nothing about egress")
    if policy["metadata"]["annotations"].get("argocd.argoproj.io/sync-wave") != "-1":
        raise Failure("the policy must be synced before the server (sync wave -1)")
    selector = match_labels(policy["spec"]["podSelector"], "the policy")
    if not selector:
        raise Failure("the policy selects every pod of the namespace")
    for pod in eng["servers"]:
        if not matches(selector, pod["labels"]):
            raise Failure(f"the policy's selector {selector} does not match the server {pod['name']}, labelled {pod['labels']}")
    for pod in eng["others"]:
        if matches(selector, pod["labels"]):
            raise Failure(f"the policy also selects {pod['name']}, which is not the server")

    served = set()
    for pod in eng["servers"]:
        served |= set(pod["ports"].values())
    listed = set().union(*(ports for ports, _ in rules(policy, server)))
    closed = set(CLOSED.get(server, {}))
    if listed & closed:
        raise Failure(f"ports {sorted(listed & closed)} are both listed and recorded as closed")
    if served - listed - closed:
        raise Failure(f"the server serves ports {sorted(served - listed - closed)} that are neither listed nor recorded as closed")
    if listed - served:
        raise Failure(f"the policy lists ports {sorted(listed - served)} the server does not serve")
    if closed - served:
        raise Failure(f"ports {sorted(closed - served)} are recorded as closed and the server does not serve them")
    name_peers = {
        match_labels(peer["namespaceSelector"], "a peer's namespace").get(NAME_LABEL)
        for _, peers in rules(policy, server) for peer in (peers or []) if "namespaceSelector" in peer
    } - {None}
    unknown = name_peers - set(layout())
    if unknown:
        raise Failure(f"the policy names namespaces {sorted(unknown)} that kernel/namespaces.yaml does not have")


def check_clients(server):
    apps = deployed()
    policy = the_policy(server, apps)
    namespaces = layout()
    wanted, refused = table(server)
    if not wanted or not refused:
        raise Failure(f"{CLIENTS.name} lists no client, or nothing to refuse, for {server}")
    problems = []
    for c in wanted:
        if c.get("evidence") not in ("proven", "inferred"):
            problems.append(f"{c['client']}: evidence must be proven or inferred")
        if not admitted(policy, server, c, namespaces):
            problems.append(f"NOT ADMITTED: {c['client']} (port {c['port']})")
    for c in refused:
        if admitted(policy, server, c, namespaces):
            problems.append(f"ADMITTED: {c['client']} (port {c['port']})")
    # Every source a rule names is somebody's, and every open port has a
    # client that needs it open.
    for ports, peers in rules(policy, server):
        ways = [None] if peers is None else list(range(len(peers)))
        for way in ways:
            for port in ports:
                if not any(c["port"] == port and admitted(policy, server, c, namespaces, only=(frozenset(ports), way)) for c in wanted):
                    what = "any source" if way is None else peers[way]
                    problems.append(f"a rule admits {what} on {port} and {CLIENTS.name} names no client that comes that way")
    # A port open to any source is open because somebody comes from where no
    # selector reaches, or from where this repository cannot say.
    for port in set().union(*(ports for ports, peers in rules(policy, server) if peers is None), set()):
        if not any(c["port"] == port and (c.get("external") or c.get("evidence") == "inferred") for c in wanted):
            problems.append(f"port {port} is open to any source and no client of the table explains it")
    if problems:
        raise Failure("\n        ".join(problems))


def operator_chart():
    out = {}
    # With the registrar on, as the bootstrap chart's Application installs it
    # (kernel/bootstrap/chart/templates/gentian-os.yaml); the chart's own
    # default leaves it out.
    docs = helm("gentian-os", "charts/gentian-os", "-n", "kernel-control", "--set", "registrar.enabled=true")
    for d in docs:
        if d.get("kind") != "Deployment":
            continue
        pod = d["spec"]["template"]
        command = pod["spec"]["containers"][0].get("command")
        out[tuple(command or ())] = (d["metadata"].get("namespace"), pod["metadata"].get("labels") or {})
    return out, docs


def check_wiring():
    namespaces = layout()
    apps = deployed()
    wanted = {s: table(s)[0] for s in SERVERS}

    def fn(name):
        return namespaces.get(name, {}).get(FUNCTION)

    wiring_kernel_postgres(apps, wanted, fn)


def wiring_kernel_postgres(apps, wanted, fn):
    kp = apps["kernel-postgres"]
    if fn(kp["namespace"]) != "data":
        raise Failure(f"kernel-postgres is deployed into {kp['namespace']}")
    if kp["params"].get("networkPolicy.enabled") != "true":
        raise Failure(f"the Application passes networkPolicy.enabled={kp['params'].get('networkPolicy.enabled')!r} by default")
    for key in ("authentication", "authorization", "control"):
        if fn(kp["params"].get(f"networkPolicy.namespaces.{key}")) != key:
            raise Failure(f"networkPolicy.namespaces.{key} is {kp['params'].get(f'networkPolicy.namespaces.{key}')!r}, not the {key} namespace")
    by_hand = policies_of(helm("server", kp["path"], "-n", kp["namespace"]))
    if [p["spec"] for p in by_hand] != [the_policy("kernel-postgres", apps)["spec"]]:
        raise Failure("the chart's own defaults render a different policy from the one the Application's parameters do")
    # Synced before the role Secrets and the Cluster, and in need of nothing
    # they wait for.
    waves = {}
    for d in render(kp):
        waves.setdefault(d["kind"], set()).add(int(d["metadata"].get("annotations", {}).get("argocd.argoproj.io/sync-wave", "0")))
    if not max(waves["NetworkPolicy"]) < min(waves["ExternalSecret"]) < min(waves["Cluster"]) < min(waves["Database"]):
        raise Failure(f"the sync waves are not policy, then Secrets, then the Cluster, then its databases: {waves}")

    # CloudNativePG: the pin the instance labels and ports were read at, and
    # its operator beside the cluster -- the status rule names no namespace.
    application = CNPG_APPLICATION.read_text()
    if f'targetRevision: "{CNPG_CHART_VERSION}"' not in application:
        raise Failure(f"the cnpg Application no longer installs chart {CNPG_CHART_VERSION}")
    if bootstrap_application("cnpg")["spec"]["destination"]["namespace"] != kp["namespace"]:
        raise Failure("CloudNativePG's operator no longer runs beside kernel-postgres; the status rule admits its own namespace only")

    # Keycloak and OpenFGA: where their values land is where the Suze claim
    # installs them, and the host they are handed is this server's.
    identity = application_set("gentian-identity")
    if identity is None:
        raise Failure("no gentian-identity ApplicationSet rendered")
    placed = {}
    for generator in identity["spec"]["generators"][0]["matrix"]["generators"]:
        for item in generator["list"]["elements"]:
            if "app" in item:
                placed[item["app"]] = item["namespace"]
    params = {p["name"]: p["value"] for p in identity["spec"]["template"]["spec"]["source"]["helm"]["parameters"]}
    host = f"kernel-postgres-rw.{kp['namespace']}.svc.cluster.local"
    if params.get("dbHost") != host:
        raise Failure(f"Keycloak and OpenFGA are handed the database host {params.get('dbHost')!r}, not {host}")
    for app, function, word in (("keycloak-idp", "authentication", "Keycloak"), ("openfga", "authorization", "OpenFGA")):
        if fn(placed.get(app)) != function:
            raise Failure(f"{app} is placed in {placed.get(app)!r}, not the {function} namespace")
        rows = [c for c in wanted["kernel-postgres"] if c["client"].startswith(word)]
        if len(rows) != 1 or rows[0]["namespace"] != placed[app] or rows[0]["port"] != CNPG_INSTANCE_PORTS["postgresql"]:
            raise Failure(f"the table's {word} is not in {placed[app]} on {CNPG_INSTANCE_PORTS['postgresql']}")
    scaffold = (ROOT / "scripts/lib/bootstrap.sh").read_text()
    for key, app in (("idpNamespace", "keycloak-idp"), ("fgaNamespace", "openfga")):
        if f"  {key}: {placed[app]}\n" not in scaffold:
            raise Failure(f"the scaffolded Suze claim does not set {key} to {placed[app]}")
    keycloak = helm("kc", "kernel/services/keycloak-idp/manifests", "-n", placed["keycloak-idp"], "--set", f"dbHost={host}")
    base = [d for d in keycloak if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "keycloak-idp-base-values"]
    database = yaml.safe_load(base[0]["data"]["values.yaml"])["database"] if base else {}
    if (database.get("hostname"), database.get("port")) != (host, CNPG_INSTANCE_PORTS["postgresql"]):
        raise Failure(f"Keycloak's values name the database {database.get('hostname')}:{database.get('port')}")
    openfga = helm("fga", "kernel/services/openfga/manifests", "-n", placed["openfga"], "--set", f"dbHost={host}")
    uri = [d for d in openfga if d.get("kind") == "ExternalSecret" and d["metadata"]["name"] == "openfga-sensitive-values"]
    if not uri or f"@{host}:{CNPG_INSTANCE_PORTS['postgresql']}/openfga" not in uri[0]["spec"]["target"]["template"]["data"]["sensitive-values.yaml"]:
        raise Failure("OpenFGA's datastore URI does not name this server on the data port")
    values = [d for d in openfga if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "openfga-base-values"]
    if yaml.safe_load(values[0]["data"]["values.yaml"])["datastore"].get("migrationType") != "initContainer":
        raise Failure("OpenFGA's migration no longer runs as an init container of its own pod: find where it runs and admit it")

    # The operator and the registrar: the labels the rule selects on, where
    # the chart puts them, and the registrar's connection string.
    programs, docs = operator_chart()
    for command, component in ((("/manager",), "operator"), (("/registrar",), "registrar")):
        if command not in programs:
            raise Failure(f"the operator chart renders no Deployment running {command[0]}")
        namespace, labels = programs[command]
        if fn(namespace) != "control":
            raise Failure(f"the {component} is rendered into {namespace}")
        rows = [c for c in wanted["kernel-postgres"] if c["podLabels"].get("app.kubernetes.io/component") == component]
        if len(rows) != 1 or rows[0]["namespace"] != namespace or not matches(rows[0]["podLabels"], labels):
            raise Failure(f"the table's {component} is not the chart's: {namespace} {labels}")
    dsn = [d for d in docs if d.get("kind") == "ExternalSecret" and d["metadata"]["name"] == "gentian-os-registrar-database"]
    if not dsn or f"@{host}:{CNPG_INSTANCE_PORTS['postgresql']}/registrar" not in str(dsn[0]["spec"]["target"]["template"]["data"]):
        raise Failure("the registrar's connection string does not name this server on the data port")
    for c in wanted["kernel-postgres"]:
        if c["client"].startswith("CloudNativePG's operator"):
            if c["namespace"] != kp["namespace"] or c["podLabels"] != CNPG_OPERATOR_LABELS or c["port"] != CNPG_INSTANCE_PORTS["status"]:
                raise Failure("the table's CloudNativePG operator is not the one pinned for the stores")



def check_off():
    # The bootstrap chart hands the switch to the kernel-postgres Application...
    for passed, want in ((None, "true"), ("true", "true"), ("false", "false")):
        extra = ["--set-string", f"storeNetworkPolicies={passed}"] if passed else []
        got = deployed(*extra)["kernel-postgres"]["params"].get("networkPolicy.enabled")
        if got != want:
            raise Failure(f"STORE_NETWORK_POLICIES={passed}: kernel-postgres is handed networkPolicy.enabled={got!r}, want {want!r}")
    # ...whose chart then admits everything. The policy is kept, not dropped:
    # that Application does not prune, so a policy no longer rendered would
    # stay as it was and the switch would do nothing.
    apps = deployed("--set-string", "storeNetworkPolicies=false")
    if apps["kernel-postgres"]["prune"] is not False:
        raise Failure("the kernel-postgres Application prunes now; say so in the chart's template, which keeps the policy for that reason")
    spec = the_policy("kernel-postgres", apps)["spec"]
    if spec.get("ingress") != [{}] or spec.get("policyTypes") != ["Ingress"]:
        raise Failure(f"switched off, kernel-postgres's policy does not admit everything: {spec.get('ingress')}")
    the_policy("kernel-postgres", deployed())


def check_modes():
    apps = deployed()
    # More than one instance changes nothing the policy selects on.
    check_shape("kernel-postgres", apps, "--set", "instances=3")


def main():
    args = sys.argv[1:]
    try:
        if args[:1] == ["shape"] and args[1:] and args[1] in SERVERS:
            check_shape(args[1])
        elif args[:1] == ["clients"] and args[1:] and args[1] in SERVERS:
            check_clients(args[1])
        elif args == ["wiring"]:
            check_wiring()
        elif args == ["off"]:
            check_off()
        elif args == ["modes"]:
            check_modes()
        else:
            print(f"usage: {sys.argv[0]} shape|clients <{'|'.join(SERVERS)}> | wiring | off | modes", file=sys.stderr)
            return 2
    except Failure as e:
        print(f"        {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
