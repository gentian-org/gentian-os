#!/usr/bin/env python3
# =============================================================================
# scripts/tests/server_network_policies.py — the NetworkPolicies on the
# servers that are not shared stores: the kernel's own PostgreSQL, the two
# mail servers and the proxy in front of them. Run by
# test-store-network-policies.sh beside the stores'.
#
# Rendered with helm and no cluster, from what a cluster would run: the
# bootstrap chart's kernel-postgres Application and the mail ApplicationSet
# (for the parameters Argo CD passes), each server's chart (for its policy
# and, where the chart builds them, its pods), and the clients from the
# `servers` section of scripts/tests/store-clients.yaml.
#
#   server_network_policies.py shape <server>     the policy against the server
#   server_network_policies.py clients <server>   the policy against the clients
#   server_network_policies.py wiring             what the policies lean on
#   server_network_policies.py off                the switch
#   server_network_policies.py modes              what a mail mode leaves out
#
# <server> is kernel-postgres, dovecot, postfix or mail-edge.
#
# Mail has ports that face the internet, and those are admitted by a rule
# with ports and no `from`. Such a rule is refused here unless every port of
# it is recorded in OPEN with the reason; so a rule that loses its `from` by
# mistake still fails. Only the proxy in the mail DMZ has any: Postfix and
# Dovecot have none, and a port of theirs that turned up open would fail.
#
# The proxy's policy is also the one that restricts egress, and `shape` holds
# it to exactly the PROXY-protocol ports of the two servers and DNS.
# =============================================================================
import re
import sys
import tempfile

import yaml

from store_network_policies import (
    CLIENTS, CNPG_APPLICATION, CNPG_CHART_VERSION, CNPG_INSTANCE_PORTS, CNPG_OPERATOR_LABELS, FUNCTION, NAME_LABEL,
    ROOT, Failure, appsets, cnpg_instance_labels, cnpg_job_labels, helm, layout, match_labels, matches,
    namespace_of, policies_of,
)

SERVERS = ("kernel-postgres", "dovecot", "postfix", "mail-edge")
MAIL = ("dovecot", "postfix", "mail-edge")
# What restricts egress as well as ingress, and is held to its egress rules.
EGRESS = ("mail-edge",)

# Ports a server's pods serve that its policy lists nowhere, and why.
CLOSED = {
    "kernel-postgres": {9187: "metrics: nothing the platform runs scrapes it"},
    "postfix": {25: "the image listens on it; mail from outside arrives on the edge port, and nothing inside the cluster is handed 25"},
    "mail-edge": {8404: "the proxy's health endpoint: the kubelet asks from the pod's own node, and nothing else has business with it"},
}

# Ports admitted from any source, and why no source can be named. A port is
# here or behind a `from`; a rule may not be open by accident.
OPEN = {
    "mail-edge": {
        2525: "inbound mail from the internet: port 25 of the load balancer",
        2587: "submission: port 587 of the load balancer, dialled by mail clients and, under the public name, by apps and Keycloak",
        2993: "IMAPS: port 993 of the load balancer, dialled by mail clients and, under the public name, by apps",
    },
}

# The image's own master.cf listens on 25 and 587 (boky/postfix, pinned with
# the chart below); the chart declares only the second.
POSTFIX_IMAGE_SMTP_PORT = 25

# Postfix's pods are built by an upstream chart that is not vendored here, so
# they cannot be rendered. What this file leans on instead is what the
# repository itself says about them: the Release that installs the chart
# (name, version and release name) and the Service of this chart that has to
# select those pods for inbound mail to work at all. `wiring` fails when the
# chart version moves, which is the moment to read its templates again:
# bokysan/mail 5.1.0 labels its pods app.kubernetes.io/name=<chart name> and
# app.kubernetes.io/instance=<release>, and declares one port, `smtp`, at
# service.port.
POSTFIX_CHART = ("mail", "5.1.0")
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


def deployed(*extra, mail="system"):
    """What Argo CD deploys for each server: the chart's path, the namespace
    and the Helm parameters. Mail is rendered in the one mode that runs it."""
    out = {}
    app = bootstrap_application("kernel-postgres", *extra)
    source = app["spec"]["source"]
    out["kernel-postgres"] = {
        "path": source["path"], "namespace": app["spec"]["destination"]["namespace"],
        "params": {p["name"]: p["value"] for p in source["helm"]["parameters"]},
        "prune": app["spec"]["syncPolicy"]["automated"]["prune"],
    }
    out.update(mail_applications(mail, *extra))
    return out


def mail_applications(mode, *extra):
    """The Applications of the mail ApplicationSet, which exists in one mail
    mode only."""
    out = {}
    mailset = application_set("gentian-mail", "--set-string", f"mailServiceMode={mode}", *extra)
    if mailset is None:
        return out
    spec = mailset["spec"]
    stage, apps = None, None
    for generator in spec["generators"][0]["matrix"]["generators"]:
        items = generator["list"]["elements"]
        if "env" in items[0]:
            stage = items[0]["env"]
        else:
            # Each chart with the namespace it is synced into.
            apps = {i["app"]: i["namespace"] for i in items}
    template = spec["template"]["spec"]

    def fill(text, name):
        return text.replace("{{.env}}", stage).replace("{{.app}}", name).replace("{{.namespace}}", apps[name])

    for name in apps:
        params = {p["name"]: fill(p["value"], name) for p in template["source"]["helm"]["parameters"]}
        out[name] = {
            "path": fill(template["source"]["path"], name), "namespace": fill(template["destination"]["namespace"], name),
            "params": params, "prune": template["syncPolicy"]["automated"]["prune"], "stage": stage,
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


def pods_of(docs):
    out = []
    for d in docs:
        if d.get("kind") not in ("Deployment", "StatefulSet", "DaemonSet", "Job"):
            continue
        template = d["spec"]["template"]
        ports = {}
        for c in template["spec"].get("initContainers", []) + template["spec"]["containers"]:
            for p in c.get("ports") or []:
                if p.get("protocol", "TCP") != "TCP":
                    raise Failure(f"{d['metadata']['name']} declares a port that is not TCP: {p}")
                ports[p.get("name", str(p["containerPort"]))] = p["containerPort"]
        out.append({"name": d["metadata"]["name"], "labels": template["metadata"].get("labels") or {}, "ports": ports})
    return out


def postfix_pod(docs):
    """Postfix's pod as this repository states it: the labels the MX Service
    selects on -- held to the Release's chart and release names -- and the
    ports that Service and the chart's values send connections to."""
    releases = [d for d in docs if d.get("kind") == "Release"]
    if len(releases) != 1:
        raise Failure("the Postfix chart does not render exactly one Release")
    chart = releases[0]["spec"]["forProvider"]["chart"]
    if (chart["name"], chart["version"]) != POSTFIX_CHART:
        raise Failure(f"the Release installs {chart['name']} {chart['version']}, not {POSTFIX_CHART}: read that chart's pod "
                      "labels and ports, then move the pin in this file")
    release = releases[0]["metadata"]["annotations"]["crossplane.io/external-name"]
    labels = {"app.kubernetes.io/name": chart["name"], "app.kubernetes.io/instance": release}
    values = {}
    for d in docs:
        if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "postfix-base-values":
            values = yaml.safe_load(d["data"]["values.yaml"])
    if not values:
        raise Failure("the Postfix chart renders no postfix-base-values")
    ports = {"submission": values["service"]["port"], "smtp": POSTFIX_IMAGE_SMTP_PORT}
    for s in (d for d in docs if d.get("kind") == "Service"):
        if s["spec"].get("type", "ClusterIP") != "ClusterIP":
            raise Failure(f"Service {s['metadata']['name']} is {s['spec']['type']}: nothing outside the cluster is to reach Postfix")
        if s["spec"]["selector"] != labels:
            raise Failure(f"Service {s['metadata']['name']} selects {s['spec']['selector']}, the Release's pods carry {labels}")
        for p in s["spec"]["ports"]:
            if p["targetPort"] != p["port"]:
                raise Failure(f"Service {s['metadata']['name']} port {p['port']} reaches the pod on {p['targetPort']}")
            ports[p["name"]] = p["port"]
    # The listeners the edge Service names are the ones the start script adds
    # to master.cf, each of them reading a PROXY header.
    script = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == f"{release}-edge-listeners"]
    if len(script) != 1:
        raise Failure("the Postfix chart renders no edge-listeners script")
    added = {int(n) for n in re.findall(r"^\s*edge_listener (\d+) \S+$", script[0]["data"]["edge-listeners.sh"], re.M)}
    edge = {port for name, port in ports.items() if name.endswith("-edge")}
    if added != edge or not edge:
        raise Failure(f"the start script adds the listeners {sorted(added)}, the edge Service names {sorted(edge)}")
    if "smtpd_upstream_proxy_protocol=haproxy" not in script[0]["data"]["edge-listeners.sh"]:
        raise Failure("the edge listeners no longer require a PROXY header: the proxy's address is inside the range Postfix trusts")
    mounts = values.get("extraVolumeMounts") or []
    if not any(m.get("mountPath", "").startswith("/docker-init.d/") and m.get("name") == "edge-listeners" for m in mounts):
        raise Failure("the edge-listeners script is not mounted where the image runs start scripts from")
    return {"name": release, "labels": labels, "ports": ports}


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
    return mail_engine(server, apps, *extra)


def mail_engine(server, apps, *extra):
    dovecot = pods_of(render(apps["dovecot"], *(extra if server == "dovecot" else ())))
    postfix = postfix_pod(render(apps["postfix"], *(extra if server == "postfix" else ())))
    edge = pods_of(render(apps["mail-edge"], *(extra if server == "mail-edge" else ())))
    if len(dovecot) != 1:
        raise Failure("the Dovecot chart does not render exactly one workload")
    if len(edge) != 1:
        raise Failure("the mail-edge chart does not render exactly one workload")
    all_ = {"dovecot": dovecot[0], "postfix": postfix, "mail-edge": edge[0]}
    others = [pod for name, pod in all_.items() if name != server]
    return {"servers": [all_[server]], "others": others + [{"name": "the installer's check", "labels": {"gentianos.io/purpose": "verify"}}]}


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
    if server in EGRESS:
        if policy["spec"].get("policyTypes") != ["Ingress", "Egress"]:
            raise Failure("the policy must restrict ingress and egress")
        check_edge_egress(policy, apps, *extra)
    elif policy["spec"].get("policyTypes") != ["Ingress"] or "egress" in policy["spec"]:
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


def edge_backends(apps, *extra):
    """Where the proxy sends each listener's connections, from its own
    configuration: {listener: (host, port)}, with every server line held to
    the PROXY protocol."""
    docs = render(apps["mail-edge"], *extra)
    conf = [d for d in docs if d.get("kind") == "ConfigMap" and "haproxy.cfg" in (d.get("data") or {})]
    if len(conf) != 1:
        raise Failure("the mail-edge chart does not render exactly one haproxy.cfg")
    text = conf[0]["data"]["haproxy.cfg"]
    out, binds, current = {}, {}, None
    for line in text.splitlines():
        words = line.split()
        if not words or words[0].startswith("#"):
            continue
        if words[0] in ("frontend", "backend", "global", "defaults", "resolvers"):
            current = (words[0], words[1] if len(words) > 1 else "")
            continue
        if current and current[0] == "frontend" and words[0] == "bind":
            binds[current[1]] = int(words[1].rsplit(":", 1)[1])
        if current and current[0] == "backend" and words[0] == "server":
            if "send-proxy-v2" not in words[3:]:
                raise Failure(f"backend {current[1]} does not send a PROXY header: the server would see the proxy's address, "
                              "which is inside the range Postfix trusts")
            if "check" in words[3:]:
                raise Failure(f"backend {current[1]} health-checks its server: HAProxy's check of a PROXY-protocol port "
                              "makes Postfix's smtpd exit and its master throttle the listener (templates/configmap.yaml)")
            host, port = words[2].rsplit(":", 1)
            out[current[1]] = (host, int(port))
    if "tls" in text.lower().replace("starttls", "") or " ssl" in text or ".pem" in text or " crt " in text:
        raise Failure("the proxy's configuration mentions TLS: it passes TLS through and holds no certificate")
    return out, binds, text


def edge_listeners(apps, *extra):
    """The proxy's listeners as its Service, its pod and its configuration
    state them, held together: {name: (public port, pod port)}."""
    docs = render(apps["mail-edge"], *extra)
    backends, binds, text = edge_backends(apps, *extra)
    pod = pods_of(docs)[0]
    services = [d for d in docs if d.get("kind") == "Service"]
    if len(services) != 1 or services[0]["spec"]["type"] != "LoadBalancer" or services[0]["spec"].get("externalTrafficPolicy") != "Local":
        raise Failure("the mail-edge chart does not render exactly one LoadBalancer Service with externalTrafficPolicy: Local")
    out = {}
    for p in services[0]["spec"]["ports"]:
        if p["targetPort"] not in pod["ports"]:
            raise Failure(f"the edge's Service sends {p['port']} to {p['targetPort']}, which the pod does not declare")
        out[p["name"]] = (p["port"], pod["ports"][p["targetPort"]])
    health = binds.pop("health", None)
    if health != pod["ports"].get("health"):
        raise Failure("the proxy's health endpoint is not the port the pod declares for it")
    if binds != {name: listen for name, (_, listen) in out.items()} or set(backends) != set(out):
        raise Failure(f"the proxy binds {binds} and has backends {sorted(backends)}; its Service publishes {out}")
    if set(pod["ports"]) - {"health"} != set(out):
        raise Failure(f"the pod declares {sorted(pod['ports'])}; the Service publishes {sorted(out)}")
    for name in out:
        if f"frontend {name}\n" not in text or "track-sc0 src" not in text.split(f"frontend {name}\n", 1)[1].split("backend ", 1)[0]:
            raise Failure(f"listener {name} counts nothing per client address")
    return out, backends


def edge_targets(apps, *extra):
    """The (namespace, pod labels, port) the proxy's configuration sends
    connections to, each resolved through the Service it names to the pods
    that Service selects."""
    _, backends = edge_listeners(apps, *extra)
    services = {}
    for server in ("postfix", "dovecot"):
        for d in render(apps[server]):
            if d.get("kind") == "Service":
                host = f"{d['metadata']['name']}.{d['metadata'].get('namespace') or apps[server]['namespace']}.svc.cluster.local"
                services[host] = (apps[server]["namespace"], d)
    out = set()
    for listener, (host, port) in backends.items():
        if host not in services:
            raise Failure(f"listener {listener} sends to {host}, which is no Service of the Postfix or Dovecot chart")
        namespace, svc = services[host]
        if svc["spec"].get("type", "ClusterIP") != "ClusterIP":
            raise Failure(f"{host} is {svc['spec']['type']}: nothing outside the cluster is to reach a mail server")
        named = [p for p in svc["spec"]["ports"] if p["port"] == port]
        if len(named) != 1 or not named[0]["name"].endswith("-edge"):
            raise Failure(f"listener {listener} sends to {host}:{port}, which is not a PROXY-protocol port of that Service")
        out.add((namespace, tuple(sorted(svc["spec"]["selector"].items())), port))
    return out


def check_edge_egress(policy, apps, *extra):
    """The proxy may open a connection to the PROXY-protocol ports its own
    configuration names, and to DNS: nothing more and nothing less."""
    wanted = edge_targets(apps, *extra)
    got, dns = set(), None
    for rule in policy["spec"].get("egress") or []:
        ports = rule.get("ports") or []
        if not ports:
            raise Failure("an egress rule with no ports opens every port")
        if "to" not in rule:
            if dns is not None or {(p.get("protocol"), p.get("port")) for p in ports} != {("UDP", 53), ("TCP", 53)}:
                raise Failure(f"an egress rule names no destination and is not DNS alone: {ports}")
            dns = rule
            continue
        if not rule["to"]:
            raise Failure("an egress rule has an empty `to`, which is everywhere")
        for peer in rule["to"]:
            if set(peer) != {"namespaceSelector", "podSelector"}:
                raise Failure(f"an egress peer names {sorted(peer)}; a mail server is a namespace AND its pods")
            namespace = match_labels(peer["namespaceSelector"], "an egress peer's namespace")
            if set(namespace) != {NAME_LABEL}:
                raise Failure(f"an egress peer selects its namespace by {namespace}, not by name")
            for p in ports:
                if p.get("protocol") != "TCP" or not isinstance(p.get("port"), int):
                    raise Failure("every egress port must be a number, named with its protocol")
                got.add((namespace[NAME_LABEL], tuple(sorted(match_labels(peer["podSelector"], "an egress peer's pods").items())), p["port"]))
    if dns is None:
        raise Failure("the proxy may not resolve the names of the servers it sends to")
    if got != wanted:
        raise Failure(f"the proxy may reach {sorted(got)}; its configuration sends to {sorted(wanted)}")


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
    wiring_mail(apps, wanted, fn)


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



def wiring_mail(apps, wanted, fn):
    if set(MAIL) - set(apps):
        raise Failure(f"the mail ApplicationSet deploys {sorted(apps)}, not {sorted(MAIL)}")
    for server in MAIL:
        app = apps[server]
        function = "mail-dmz" if server == "mail-edge" else "mail"
        if fn(app["namespace"]) != function or app["params"].get("servicesNamespace") != app["namespace"]:
            raise Failure(f"{server} is deployed into {app['namespace']} with servicesNamespace={app['params'].get('servicesNamespace')!r}")
        # Each chart is told where the other side of its rules is, by the
        # namespace the ApplicationSet really syncs that side into.
        if app["params"].get("edge.namespace") != apps["mail-edge"]["namespace"]:
            raise Failure(f"{server} is told the mail edge is in {app['params'].get('edge.namespace')!r}")
        if app["params"].get("mailNamespace") != apps["postfix"]["namespace"] or apps["postfix"]["namespace"] != apps["dovecot"]["namespace"]:
            raise Failure(f"{server} is told the mail servers are in {app['params'].get('mailNamespace')!r}")
        if app["params"].get("networkPolicy.enabled") != "true":
            raise Failure(f"{server}: the ApplicationSet passes networkPolicy.enabled={app['params'].get('networkPolicy.enabled')!r} by default")
        by_hand = policies_of(helm("server", app["path"], "--set", f"servicesNamespace={app['namespace']}", "--set", f"env={app['stage']}"))
        if [p["spec"] for p in by_hand] != [the_policy(server, apps)["spec"]]:
            raise Failure(f"{server}: the chart's own defaults render a different policy from the one the ApplicationSet's parameters do")
        for c in wanted[server]:
            if not c.get("external") and isinstance(c["namespace"], str) and c["podLabels"].get("app.kubernetes.io/name") in ("mail", "dovecot") \
                    and c["namespace"] != apps["postfix"]["namespace"]:
                raise Failure(f"{c['client']}: the table puts a mail server's pod in {c['namespace']}")
    wiring_mail_edge(apps, wanted)
    # Postfix reaches Dovecot where the policy admits it: by the Service of
    # the same namespace, on the two ports the rule names.
    postfix_docs = render(apps["postfix"])
    settings = yaml.safe_load([d for d in postfix_docs if d.get("kind") == "ConfigMap"
                               and d["metadata"]["name"] == "postfix-base-values"][0]["data"]["values.yaml"])["config"]["postfix"]
    dovecot_docs = render(apps["dovecot"])
    dovecot = pods_of(dovecot_docs)[0]
    service = f"dovecot-{apps['dovecot']['stage']}.{apps['dovecot']['namespace']}.svc.cluster.local"
    if settings["virtual_transport"] != f"lmtp:[{service}]:{dovecot['ports']['lmtp']}":
        raise Failure(f"Postfix delivers to {settings['virtual_transport']}")
    if settings["smtpd_sasl_path"] != f"inet:{service}:{dovecot['ports']['sasl']}":
        raise Failure(f"Postfix authenticates against {settings['smtpd_sasl_path']}")
    for s in (d for d in dovecot_docs if d.get("kind") == "Service"):
        for p in s["spec"]["ports"]:
            if dovecot["ports"].get(p["targetPort"]) != p["port"]:
                raise Failure(f"Service {s['metadata']['name']} port {p['port']} reaches the pod on {dovecot['ports'].get(p['targetPort'])}")
    pf = postfix_pod(postfix_docs)
    for c in wanted["dovecot"]:
        if c["client"].startswith("Postfix") and c["podLabels"] != pf["labels"]:
            raise Failure(f"{c['client']}: the table says {c['podLabels']}, Postfix's pods carry {pf['labels']}")
    # The ports the conf file listens on are the ones the Deployment declares.
    conf = [d for d in dovecot_docs if d.get("kind") == "ConfigMap" and "dovecot.conf" in (d.get("data") or {})][0]["data"]["dovecot.conf"]
    listening = {int(n) for n in re.findall(r"^\s*port = (\d+)\s*$", conf, re.M)} - {0}
    if listening != set(dovecot["ports"].values()):
        raise Failure(f"dovecot.conf listens on {sorted(listening)}, the Deployment declares {sorted(dovecot['ports'].values())}")
    # The installer's check runs beside the server it probes, with the label
    # the rule admits.
    verify = (ROOT / "scripts/lib/verify-kernel-services.sh").read_text()
    if '"gentianos.io/purpose": "verify"' not in verify or '_verify_tcp_from_cluster "${ns}" "${fqdn}" "${port}"' not in verify \
            or 'local ns="${VERIFY_POD_NAMESPACE:-$1}"' not in verify:
        raise Failure("verify_dovecot_installation no longer runs its probe, labelled gentianos.io/purpose=verify, in the namespace it checks")


def wiring_mail_edge(apps, wanted):
    """The proxy and the two servers agree about each other: the proxy sends
    to Services that exist, on ports that read a PROXY header; each server
    admits exactly the proxy's pods there; and nothing of the three but the
    proxy has a load balancer."""
    listeners, backends = edge_listeners(apps)
    edge_pod = pods_of(render(apps["mail-edge"]))[0]
    edge_ns = apps["mail-edge"]["namespace"]
    targets = edge_targets(apps)
    for server in ("dovecot", "postfix"):
        docs = render(apps[server])
        if any(d.get("kind") == "Service" and d["spec"].get("type", "ClusterIP") != "ClusterIP" for d in docs):
            raise Failure(f"the {server} chart renders a Service that is not ClusterIP")
        policy = the_policy(server, apps)
        mine = {port for ns, _, port in targets if any(
            matches(dict(sel), pod["labels"]) for _, sel, p in targets if p == port for pod in engine(server, apps)["servers"])}
        admitted_ports = set()
        for ports, peers in rules(policy, server):
            for peer in peers or []:
                if match_labels(peer.get("namespaceSelector", {}), "a peer's namespace").get(NAME_LABEL) != edge_ns:
                    continue
                if not matches(match_labels(peer.get("podSelector", {}), "a peer's pods"), edge_pod["labels"]) or "podSelector" not in peer:
                    raise Failure(f"{server} admits the mail DMZ without naming the proxy's pods, labelled {edge_pod['labels']}")
                admitted_ports |= ports
        if admitted_ports != mine or not mine:
            raise Failure(f"{server} admits the proxy on {sorted(admitted_ports)}; the proxy sends to it on {sorted(mine)}")
        for c in wanted[server]:
            if c["podLabels"].get("app.kubernetes.io/name") == "mail-edge" and (c["namespace"] != edge_ns or c["podLabels"] != edge_pod["labels"]):
                raise Failure(f"{c['client']}: the table says {c['namespace']} {c['podLabels']}, the proxy is in {edge_ns} with {edge_pod['labels']}")
    # Dovecot's edge listener reads a PROXY header and serves TLS itself.
    conf = [d for d in render(apps["dovecot"]) if d.get("kind") == "ConfigMap" and "dovecot.conf" in (d.get("data") or {})][0]["data"]["dovecot.conf"]
    block = re.search(r"inet_listener imaps-edge \{(.*?)\}", conf, re.S)
    if not block or "haproxy = yes" not in block.group(1) or "ssl = yes" not in block.group(1) \
            or f"port = {backends['imaps'][1]}" not in block.group(1):
        raise Failure("Dovecot's imaps-edge listener is not the TLS, PROXY-protocol port the proxy sends IMAPS to")
    if len(re.findall(r"^\s*haproxy = yes\s*$", conf, re.M)) != 1 or "haproxy_trusted_networks" not in conf:
        raise Failure("exactly one Dovecot listener may read a PROXY header, and haproxy_trusted_networks has to be set for it")
    # The public ports are the three the operator's DNS names stand for.
    if {name: public for name, (public, _) in listeners.items()} != {"smtp": 25, "submission": 587, "imaps": 993}:
        raise Failure(f"the mail edge publishes {listeners}")
    dns = (ROOT / "internal/controller/mail_dnsendpoint.go").read_text()
    if "mailEdgeSMTPPort  int32 = 25" not in dns or "mailEdgeIMAPSPort int32 = 993" not in dns or '"mail-edge-"+' not in dns:
        raise Failure("the operator no longer reads the mail addresses from the Service mail-edge-<stage>, ports 25 and 993")
    service = [d for d in render(apps["mail-edge"]) if d.get("kind") == "Service"][0]
    if service["metadata"]["name"] != f"mail-edge-{apps['mail-edge']['stage']}":
        raise Failure(f"the edge's Service is called {service['metadata']['name']}")
    # The pod as the DMZ requires it: no token, an ordinary user, nothing writable.
    deployment = [d for d in render(apps["mail-edge"]) if d.get("kind") == "Deployment"][0]["spec"]["template"]["spec"]
    container = deployment["containers"][0]
    sc = container.get("securityContext") or {}
    if deployment.get("automountServiceAccountToken") is not False or not deployment["securityContext"].get("runAsNonRoot") \
            or sc.get("readOnlyRootFilesystem") is not True or sc.get("allowPrivilegeEscalation") is not False \
            or (sc.get("capabilities") or {}).get("drop") != ["ALL"]:
        raise Failure("the proxy's pod must carry no service-account token, run as an ordinary user without privilege "
                      "escalation or capabilities, and have a read-only root filesystem")
    if any("secret" in v or "persistentVolumeClaim" in v for v in deployment.get("volumes") or []):
        raise Failure("the proxy mounts a Secret or a volume claim: the DMZ holds no key and no data")
    if not re.fullmatch(r"haproxy:[0-9][^@]*@sha256:[0-9a-f]{64}", container["image"]):
        raise Failure(f"the proxy's image is {container['image']}, not pinned by tag and digest")
    if any(v.get("valueFrom") for v in container.get("env") or []) or container.get("envFrom"):
        raise Failure("the proxy takes a value from a Secret or a ConfigMap by reference")


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
    off_mail()


def off_mail():
    # The mail charts are pruned, and render none.
    for extra in (["--set-string", "storeNetworkPolicies=false"], ["--set", "storeNetworkPolicies=false"]):
        apps = deployed(*extra)
        for server in MAIL:
            if apps[server]["params"].get("networkPolicy.enabled") != "false":
                raise Failure(f"{server}: switched off with {extra[0]}, the ApplicationSet still passes "
                              f"networkPolicy.enabled={apps[server]['params'].get('networkPolicy.enabled')!r}")
            if apps[server]["prune"] is not True:
                raise Failure(f"{server}: its Application does not prune, so a policy no longer rendered would stay")
            if policies_of(render(apps[server])):
                raise Failure(f"{server}: a NetworkPolicy is rendered with the switch off")
    apps = deployed()
    for server in MAIL:
        the_policy(server, apps)


def check_modes():
    apps = deployed()
    # More than one instance changes nothing the policy selects on.
    check_shape("kernel-postgres", apps, "--set", "instances=3")
    modes_mail(apps)


def modes_mail(apps):
    # A cluster that relays its mail runs no mail server, and so no policy.
    for mode in ("external", "none"):
        if mail_applications(mode):
            raise Failure(f"mail.serviceMode={mode}: the mail ApplicationSet is still rendered")
    # Dovecot without a certificate serves no IMAPS, on either port, and the
    # policy lists neither.
    check_shape("dovecot", apps, "--set", "tls.secretName=")
    listed = set().union(*(p for p, _ in rules(the_policy("dovecot", apps, "--set", "tls.secretName="), "dovecot")))
    if listed & {993, 10993}:
        raise Failure(f"Dovecot without a certificate still lists {sorted(listed & {993, 10993})}")
    # A listener of the edge that is switched off is gone from the load
    # balancer, from the proxy and from both directions of its policy.
    for name, pod_port, backend_port in (("smtp", 2525, 10025), ("submission", 2587, 10587), ("imaps", 2993, 10993)):
        off = ("--set", f"listeners.{name}.enabled=false")
        check_shape("mail-edge", apps, *off)
        policy = the_policy("mail-edge", apps, *off)
        if pod_port in set().union(*(p for p, _ in rules(policy, "mail-edge"))):
            raise Failure(f"the edge without its {name} listener still admits {pod_port}")
        if any(p.get("port") == backend_port for rule in policy["spec"]["egress"] for p in rule["ports"]):
            raise Failure(f"the edge without its {name} listener may still reach {backend_port}")
        if name in edge_listeners(apps, *off)[0]:
            raise Failure(f"the edge without its {name} listener still publishes it")
    # With none, there is no proxy, no load balancer and no policy.
    none = [a for n in ("smtp", "submission", "imaps") for a in ("--set", f"listeners.{n}.enabled=false")]
    left = [d["kind"] for d in render(apps["mail-edge"], *none)]
    if left:
        raise Failure(f"the edge with every listener off still renders {left}")
    # Behind a load balancer that names the client itself, the proxy takes a
    # PROXY header from that load balancer and from nobody else.
    text = edge_backends(apps, "--set", "loadBalancer.proxyProtocol.enabled=true", "--set", "loadBalancer.proxyProtocol.from={192.0.2.7/32}")[2]
    if text.count("tcp-request connection expect-proxy layer4 if { src 192.0.2.7/32 }") != 3 or "accept-proxy" in text:
        raise Failure("with proxyProtocol.from set, each listener must expect a PROXY header from those addresses and from no other")
    text = edge_backends(apps, "--set", "loadBalancer.proxyProtocol.enabled=true")[2]
    if text.count(" accept-proxy") != 3:
        raise Failure("with proxyProtocol.enabled and no `from`, each listener must require a PROXY header")
    text = edge_backends(apps)[2]
    if "accept-proxy" in text or "expect-proxy" in text:
        raise Failure("by default the proxy must take a PROXY header from nobody: any client could then name its own address")


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
