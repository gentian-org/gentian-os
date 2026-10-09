#!/usr/bin/env python3
# =============================================================================
# scripts/tests/llm_network_policies.py — the model gateway's namespace, held
# to what store_network_policies.py holds the four stores to.
#
# The namespace is not one server but four pod sets -- the gateway, its
# PostgreSQL, its Redis and the mock model server -- and each carries a
# NetworkPolicy of its own. Rendered with helm, no cluster: the LLM
# ApplicationSet (for the parameters Argo CD passes the chart) and the chart
# (for the pods and the policies). The clients are the rows of
# scripts/tests/store-clients.yaml whose store is `llm`, each naming the
# `server` it connects to.
#
#   llm_network_policies.py shape     each policy against its server
#   llm_network_policies.py clients   each policy against its clients
#   llm_network_policies.py wiring    what the policies lean on
#   llm_network_policies.py off       the switch, and the mock
#   llm_network_policies.py console   the gateway's console, off unless asked for
# =============================================================================
import sys
import tempfile

import yaml

from store_network_policies import (CLIENTS, CNPG_INSTANCE_PORTS, FUNCTION, NAME_LABEL, ROOT, Failure, admitted, appsets,
                                    cnpg_instance_labels, cnpg_job_labels, helm, layout, match_labels, matches,
                                    namespace_of, policies_of, rules)

STORE = "llm"
SERVERS = ("gateway", "database", "cache", "mock")

# The policy that guards each server, by name.
POLICY = {
    "gateway": "llm-gateway-ingress",
    "database": "llm-database-ingress",
    "cache": "llm-cache-ingress",
    "mock": "llm-mock-ingress",
}

# The workload each server is, in the chart.
WORKLOAD = {
    "gateway": ("Deployment", "litellm-proxy"),
    "database": ("Cluster", "litellm-db"),
    "cache": ("Deployment", "redis-llm"),
    "mock": ("Deployment", "vllm-inference"),
}

# Ports a server's pods declare that no policy lists, and why.
CLOSED = {
    "database": {9187: "metrics: nothing the platform runs scrapes it"},
}

# The Helm value that says the claim switched the gateway's console on
# (spec.llm.console.enabled), as the ApplicationSets take it. A row of the
# table marked `when: console` is a client only then.
CONSOLE_ON = ("--set-string", "llmConsoleEnabled=true")

# The port an app is handed and its kernel-access policy opens
# (internal/modelgateway Port; the Go test of the client table holds the
# table's row to it).
GATEWAY_PORT = 4000


def application(*extra):
    """What the LLM ApplicationSet deploys: the chart's path, its namespace
    and the Helm parameters Argo CD passes it."""
    sets = [d for d in appsets("--set-string", "llmEnabled=true", *extra)
            if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == "gentian-llm"]
    if len(sets) != 1:
        raise Failure("no gentian-llm ApplicationSet rendered for a cluster that serves models")
    spec = sets[0]["spec"]
    stage, app = None, None
    for generator in spec["generators"][0]["matrix"]["generators"]:
        item = generator["list"]["elements"][0]
        stage = item.get("env", stage)
        app = item.get("app", app)
    template = spec["template"]["spec"]
    # Two sources: the chart, and the deployments repository the Cluster claim
    # is handed to the chart from, as a values file.
    charts = [s for s in template["sources"] if "path" in s]
    refs = [s for s in template["sources"] if "ref" in s]
    if len(charts) != 1 or len(refs) != 1 or len(template["sources"]) != 2:
        raise Failure("the gentian-llm ApplicationSet no longer has one chart source and one reference source")
    chart = charts[0]
    params = {p["name"]: p["value"].replace("{{.env}}", stage) for p in chart["helm"]["parameters"]}
    return {
        "path": chart["path"].replace("{{.app}}", app),
        "namespace": template["destination"]["namespace"],
        "params": params,
        "valueFiles": chart["helm"].get("valueFiles") or [],
        "reference": refs[0],
    }


def render(app, *extra):
    args = ["llm", app["path"], "-n", app["namespace"]]
    for name, value in app["params"].items():
        args += ["--set", f"{name}={value}"]
    return helm(*args, *extra)


def pods(docs):
    """Every pod set the chart starts: name -> labels and declared ports."""
    out = {}
    for d in docs:
        kind, name = d.get("kind"), d["metadata"]["name"]
        if kind == "Cluster":
            # CloudNativePG builds these pods, not the chart: pinned, as for
            # the tenants' PostgreSQL (store_network_policies.py).
            out[(kind, name)] = {"labels": cnpg_instance_labels(name), "ports": dict(CNPG_INSTANCE_PORTS)}
            out[("Job", name + "-1-initdb")] = {"labels": cnpg_job_labels(name), "ports": {}}
            continue
        if kind not in ("Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"):
            continue
        template = d["spec"]["jobTemplate"]["spec"]["template"] if kind == "CronJob" else d["spec"]["template"]
        ports = {}
        for c in template["spec"].get("initContainers", []) + template["spec"]["containers"]:
            for p in c.get("ports") or []:
                if p.get("protocol", "TCP") != "TCP":
                    raise Failure(f"{name} declares a port that is not TCP: {p}")
                ports[p.get("name", str(p["containerPort"]))] = p["containerPort"]
        out[(kind, name)] = {"labels": template["metadata"].get("labels") or {}, "ports": ports}
    return out


def by_name(docs):
    found = {p["metadata"]["name"]: p for p in policies_of(docs)}
    return found


def check_shape():
    app = application()
    docs = render(app)
    workloads = pods(docs)
    policies = by_name(docs)
    if set(policies) != set(POLICY.values()):
        raise Failure(f"the chart renders the policies {sorted(policies)}, want {sorted(POLICY.values())}")

    # Every pod set that listens is somebody's server: one the chart gains
    # without a policy would be open while the namespace reads as closed.
    serving = {key for key, pod in workloads.items() if pod["ports"]}
    if serving != set(WORKLOAD.values()):
        raise Failure(f"the chart's listening pod sets are {sorted(serving)}; this test knows {sorted(WORKLOAD.values())}")

    services = [d for d in docs if d.get("kind") == "Service"]
    for server in SERVERS:
        policy, pod = policies[POLICY[server]], workloads[WORKLOAD[server]]
        where = f"{POLICY[server]}:"
        if policy["metadata"].get("namespace") != app["namespace"]:
            raise Failure(f"{where} it is in {policy['metadata'].get('namespace')}, the server in {app['namespace']}")
        if policy["spec"].get("policyTypes") != ["Ingress"]:
            raise Failure(f"{where} it must restrict ingress and say nothing about egress")
        if (policy["metadata"].get("annotations") or {}).get("argocd.argoproj.io/sync-wave") != "-1":
            raise Failure(f"{where} it is not synced before the server it guards (sync wave -1)")
        selector = match_labels(policy["spec"]["podSelector"], where)
        if not selector:
            raise Failure(f"{where} it selects every pod of the namespace")
        if not matches(selector, pod["labels"]):
            raise Failure(f"{where} its selector {selector} does not match the server, labelled {pod['labels']}")
        for key, other in workloads.items():
            if key != WORKLOAD[server] and matches(selector, other["labels"]):
                raise Failure(f"{where} it also selects {key[1]}, which is not its server")

        served = set(pod["ports"].values())
        listed = set().union(*(ports for ports, _ in rules(policy)))
        closed = set(CLOSED.get(server, {}))
        if listed & closed:
            raise Failure(f"{where} ports {sorted(listed & closed)} are both listed and recorded as closed")
        if served - listed - closed:
            raise Failure(f"{where} the server declares ports {sorted(served - listed - closed)} that are neither listed nor recorded as closed")
        if listed - served:
            raise Failure(f"{where} it lists ports {sorted(listed - served)} the server does not declare")
        if closed - served:
            raise Failure(f"{where} ports {sorted(closed - served)} are recorded as closed and the server does not declare them")

        # A Service that renames a port would make the policy's number the
        # wrong one: Calico matches the pod's port.
        for s in services:
            if not matches(s["spec"].get("selector") or {"<none>": ""}, pod["labels"]):
                continue
            for p in s["spec"]["ports"]:
                target = p.get("targetPort", p["port"])
                if pod["ports"].get(target, target) != p["port"]:
                    raise Failure(f"Service {s['metadata']['name']} port {p['port']} reaches the pod on {pod['ports'].get(target, target)}")

        name_peers = {
            match_labels(peer["namespaceSelector"], "a peer's namespace").get(NAME_LABEL)
            for _, peers in rules(policy) for peer in peers if "namespaceSelector" in peer
        } - {None}
        unknown = name_peers - set(layout())
        if unknown:
            raise Failure(f"{where} it names namespaces {sorted(unknown)} that kernel/namespaces.yaml does not have")


def check_clients():
    namespaces = layout()
    table = yaml.safe_load(CLIENTS.read_text())
    problems = []
    for row in table["clients"] + table["denied"]:
        if row["store"] == STORE and row.get("server") not in SERVERS:
            problems.append(f"{row['client']}: names no server of {SERVERS}")
        if row["store"] == STORE and row.get("when", "console") != "console":
            problems.append(f"{row['client']}: `when: {row['when']}` is not a condition this test knows")
    conditional = [c for c in table["clients"] if c["store"] == STORE and c.get("when") == "console"]
    if [c["server"] for c in conditional] != ["gateway"]:
        problems.append(f"{CLIENTS.name} must name exactly one client that comes with the console, at the gateway")
    if problems:
        raise Failure("\n        ".join(problems))
    # Two clusters: one whose claim says nothing about the console, and one
    # that switches it on. A client that comes with the console is admitted
    # on the second and refused on the first.
    for console, extra in ((False, ()), (True, CONSOLE_ON)):
        policies = by_name(render(application(*extra)))
        said = "the console on" if console else "the console off"
        for server in SERVERS:
            policy = policies[POLICY[server]]
            rows = [c for c in table["clients"] if c["store"] == STORE and c["server"] == server]
            wanted = [c for c in rows if console or c.get("when") != "console"]
            refused = [c for c in table["denied"] if c["store"] == STORE and c["server"] == server]
            refused += [c for c in rows if c.get("when") == "console" and not console]
            if not wanted or not refused:
                raise Failure(f"{CLIENTS.name} lists no client, or nothing to refuse, for the {server} of {STORE}")
            for c in wanted:
                name, labels = namespace_of(c, namespaces)
                if not admitted(policy, name, labels, c.get("podLabels") or {}, c["port"]):
                    problems.append(f"NOT ADMITTED to the {server} with {said}: {c['client']} ({name}, port {c['port']})")
            for c in refused:
                name, labels = namespace_of(c, namespaces)
                if admitted(policy, name, labels, c.get("podLabels") or {}, c["port"]):
                    problems.append(f"ADMITTED to the {server} with {said}: {c['client']} ({name}, port {c['port']})")
            # Every source a rule names is somebody's.
            for ports, peers in rules(policy):
                for peer in peers:
                    one = {"metadata": policy["metadata"],
                           "spec": {"ingress": [{"from": [peer], "ports": [{"protocol": "TCP", "port": p} for p in ports]}]}}
                    used = False
                    for c in wanted:
                        name, labels = namespace_of(c, namespaces)
                        used = used or admitted(one, name, labels, c.get("podLabels") or {}, c["port"])
                    if not used:
                        problems.append(f"{POLICY[server]} admits {peer} on {sorted(ports)} with {said} and {CLIENTS.name} names no client that comes that way")
    if problems:
        raise Failure("\n        ".join(problems))


def check_wiring():
    namespaces = layout()
    app = application()
    params = app["params"]
    if namespaces.get(app["namespace"], {}).get(FUNCTION) != STORE:
        raise Failure(f"the LLM ApplicationSet deploys into {app['namespace']}, which is not the llm namespace")
    if params.get("servicesNamespace") != app["namespace"]:
        raise Failure(f"servicesNamespace is {params.get('servicesNamespace')!r}, the destination {app['namespace']!r}")
    if params.get("networkPolicy.enabled") != "true":
        raise Failure(f"the ApplicationSet passes networkPolicy.enabled={params.get('networkPolicy.enabled')!r} by default")
    for key in ("control", "data", "edge"):
        value = params.get(f"networkPolicy.namespaces.{key}")
        if namespaces.get(value, {}).get(FUNCTION) != key:
            raise Failure(f"networkPolicy.namespaces.{key} is {value!r}, which is not the {key} namespace")
    # The chart rendered by hand into the same namespace, with its own
    # defaults, is the same set of policies.
    by_hand = by_name(helm("llm", app["path"], "-n", app["namespace"], "--set", f"servicesNamespace={app['namespace']}"))
    passed = by_name(render(app))
    if {n: p["spec"] for n, p in by_hand.items()} != {n: p["spec"] for n, p in passed.items()}:
        raise Failure("the chart's own defaults render different policies from the ones the ApplicationSet's parameters do")
    # The gateway's port is the one an app is handed.
    gateway = passed[POLICY["gateway"]]
    if set().union(*(ports for ports, _ in rules(gateway))) != {GATEWAY_PORT}:
        raise Failure(f"the gateway's policy does not admit exactly port {GATEWAY_PORT}")
    table = yaml.safe_load(CLIENTS.read_text())
    apps = [c for c in table["clients"] if c["store"] == STORE and c.get("built") == "tenant-app-llm"]
    if len(apps) != 1 or apps[0]["port"] != GATEWAY_PORT or apps[0]["server"] != "gateway":
        raise Failure("the table does not name the declaring app as a client of the gateway on its port")
    # The Envoy pods carry the label the rule selects them by: the edge's own
    # chart spreads them by it.
    proxy = (ROOT / "kernel/manifests/gateway/chart/templates/envoyproxy.yaml").read_text()
    if "app.kubernetes.io/name: envoy" not in proxy:
        raise Failure("the edge chart no longer selects the Gateway's pods by app.kubernetes.io/name=envoy; "
                      "read what Envoy Gateway labels them with before trusting the gateway's rule for the edge")
    # v4 puts the chart in the namespace everything shares; no policy there.
    if policies_of(helm("llm", app["path"])):
        raise Failure("the chart renders NetworkPolicies on the v4 layout, where the clients are not where the rules say")


def check_off():
    for extra in (["--set-string", "storeNetworkPolicies=false"], ["--set", "storeNetworkPolicies=false"]):
        app = application(*extra)
        if app["params"].get("networkPolicy.enabled") != "false":
            raise Failure(f"switched off with {extra[0]}, the ApplicationSet still passes "
                          f"networkPolicy.enabled={app['params'].get('networkPolicy.enabled')!r}")
        left = policies_of(render(app))
        if left:
            raise Failure("NetworkPolicies are rendered with the switch off")
    # A cluster with GPUs runs no mock model server, and no policy for one.
    # Whether it has GPUs is the claim's to say, which the chart is handed.
    docs = render(application(), "--set", "spec.llm.gpuAcceleration=true")
    if WORKLOAD["mock"] in pods(docs):
        raise Failure("the mock model server is rendered on a cluster with GPUs")
    if set(by_name(docs)) != set(POLICY.values()) - {POLICY["mock"]}:
        raise Failure(f"with GPUs the chart renders the policies {sorted(by_name(docs))}")
    # A cluster that serves no models has no ApplicationSet at all.
    if [d for d in appsets() if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == "gentian-llm"]:
        raise Failure("the LLM ApplicationSet is rendered for a cluster that serves no models")


def edge_peers(policy, edge):
    """The sources a policy admits from the edge namespace."""
    return [peer for _, peers in rules(policy) for peer in peers
            if match_labels(peer.get("namespaceSelector") or {}, "a peer's namespace").get(NAME_LABEL) == edge]


def check_console():
    """The gateway's console is a claim decision, off unless the claim says
    true: without it nothing of the edge reaches the gateway."""
    edge = [name for name, labels in layout().items() if labels.get(FUNCTION) == "edge"]
    if len(edge) != 1:
        raise Failure(f"the layout has the edge namespaces {edge}")
    edge = edge[0]
    # What the ApplicationSet passes the chart: "true" for the one word that
    # switches the console on, "false" for everything else.
    for extra, want in (((), "false"), (("--set-string", "llmConsoleEnabled=false"), "false"),
                        (("--set-string", "llmConsoleEnabled="), "false"), (("--set-string", "llmConsoleEnabled=yes"), "false"),
                        (CONSOLE_ON, "true"), (("--set", "llmConsoleEnabled=true"), "true")):
        app = application(*extra)
        got = app["params"].get("console.enabled")
        if got != want:
            raise Failure(f"with {' '.join(extra) or 'nothing said'} the ApplicationSet passes console.enabled={got!r}, want {want!r}")
        gateway = by_name(render(app))[POLICY["gateway"]]
        peers = edge_peers(gateway, edge)
        if want == "false" and peers:
            raise Failure(f"with {' '.join(extra) or 'nothing said'} the gateway admits the edge: {peers}")
        if want == "true":
            if len(peers) != 1 or match_labels(peers[0].get("podSelector") or {}, "the edge's pods") != {"app.kubernetes.io/name": "envoy"}:
                raise Failure(f"with the console on the gateway must admit the Gateway's Envoy pods and nothing else of the edge: {peers}")
    # The chart's own default is off as well: rendered by hand, no edge.
    app = application()
    by_hand = by_name(helm("llm", app["path"], "-n", app["namespace"], "--set", f"servicesNamespace={app['namespace']}"))
    if edge_peers(by_hand[POLICY["gateway"]], edge):
        raise Failure("the chart's own defaults admit the edge to the gateway")
    # No other server of the namespace is the edge's business, either way.
    for extra in ((), CONSOLE_ON):
        policies = by_name(render(application(*extra)))
        for server in SERVERS:
            if server != "gateway" and edge_peers(policies[POLICY[server]], edge):
                raise Failure(f"{POLICY[server]} admits the edge")
    # The bootstrap chart hands the claim's answer to the ApplicationSets, off
    # unless it is the word true.
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        f.write("namespaces:\n")
        for line in (ROOT / "kernel/namespaces.yaml").read_text().splitlines():
            f.write("  " + line + "\n")
        f.flush()
        base = ["boot", "kernel/bootstrap/chart", "-f", f.name, "-f", "kernel/platforms.yaml",
                "--set-string", "appsets.enabled=true", "--set-string", "kernelDomain=k.example", "--set-string", "cluster=c",
                "--set-string", "llmEnabled=true",
                "--set-string", "versions.headlamp.chart=0.0.0", "--set-string", "versions.headlamp.repo=https://example.invalid"]
        for passed, want in ((None, "false"), ("false", "false"), ("", "false"), ("true", "true")):
            args = base + (["--set-string", f"llmConsoleEnabled={passed}"] if passed is not None else [])
            root = [d for d in helm(*args) if d.get("kind") == "Application" and d["metadata"]["name"] == "gentian-appsets"]
            if len(root) != 1:
                raise Failure("the bootstrap chart renders no gentian-appsets Application")
            got = root[0]["spec"]["source"]["helm"]["valuesObject"].get("llmConsoleEnabled")
            if got != want:
                raise Failure(f"LLM_CONSOLE={passed!r}: the ApplicationSets are handed llmConsoleEnabled={got!r}, want {want!r}")
    # And the installer passes the claim's answer, defaulting to off.
    step = (ROOT / "scripts/steps/B-01-bootstrap-apps.sh").read_text()
    if '--set-string "llmConsoleEnabled=${LLM_CONSOLE:-false}"' not in step:
        raise Failure("B-01 no longer passes the claim's llm.console.enabled to the bootstrap chart as llmConsoleEnabled")


def main():
    checks = {"shape": check_shape, "clients": check_clients, "wiring": check_wiring, "off": check_off,
              "console": check_console}
    args = sys.argv[1:]
    if len(args) != 1 or args[0] not in checks:
        print(f"usage: {sys.argv[0]} {'|'.join(checks)}", file=sys.stderr)
        return 2
    try:
        checks[args[0]]()
    except Failure as e:
        print(f"        {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
