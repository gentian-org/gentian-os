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
# =============================================================================
import sys

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
    params = {p["name"]: p["value"].replace("{{.env}}", stage) for p in template["source"]["helm"]["parameters"]}
    return {
        "path": template["source"]["path"].replace("{{.app}}", app),
        "namespace": template["destination"]["namespace"],
        "params": params,
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
    app = application()
    policies = by_name(render(app))
    namespaces = layout()
    table = yaml.safe_load(CLIENTS.read_text())
    problems = []
    for row in table["clients"] + table["denied"]:
        if row["store"] == STORE and row.get("server") not in SERVERS:
            problems.append(f"{row['client']}: names no server of {SERVERS}")
    if problems:
        raise Failure("\n        ".join(problems))
    for server in SERVERS:
        policy = policies[POLICY[server]]
        wanted = [c for c in table["clients"] if c["store"] == STORE and c["server"] == server]
        refused = [c for c in table["denied"] if c["store"] == STORE and c["server"] == server]
        if not wanted or not refused:
            raise Failure(f"{CLIENTS.name} lists no client, or nothing to refuse, for the {server} of {STORE}")
        for c in wanted:
            name, labels = namespace_of(c, namespaces)
            if not admitted(policy, name, labels, c.get("podLabels") or {}, c["port"]):
                problems.append(f"NOT ADMITTED to the {server}: {c['client']} ({name}, port {c['port']})")
        for c in refused:
            name, labels = namespace_of(c, namespaces)
            if admitted(policy, name, labels, c.get("podLabels") or {}, c["port"]):
                problems.append(f"ADMITTED to the {server}: {c['client']} ({name}, port {c['port']})")
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
                    problems.append(f"{POLICY[server]} admits {peer} on {sorted(ports)} and {CLIENTS.name} names no client that comes that way")
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
    app = application("--set-string", "llmGpuAcceleration=true")
    docs = render(app)
    if WORKLOAD["mock"] in pods(docs):
        raise Failure("the mock model server is rendered on a cluster with GPUs")
    if set(by_name(docs)) != set(POLICY.values()) - {POLICY["mock"]}:
        raise Failure(f"with GPUs the chart renders the policies {sorted(by_name(docs))}")
    # A cluster that serves no models has no ApplicationSet at all.
    if [d for d in appsets() if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == "gentian-llm"]:
        raise Failure("the LLM ApplicationSet is rendered for a cluster that serves no models")


def main():
    checks = {"shape": check_shape, "clients": check_clients, "wiring": check_wiring, "off": check_off}
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
