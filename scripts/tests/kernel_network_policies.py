#!/usr/bin/env python3
# =============================================================================
# scripts/tests/kernel_network_policies.py — the assertions behind
# test-kernel-network-policies.sh.
#
# The kernel namespaces refuse an ingress nothing lists, and the list is
# internal/kernel/kernelnet/inventory.yaml. So a pod set, a port or a webhook
# that is deployed and not listed is one the next install finds out about as a
# timeout. These hold what the repository deploys into a kernel namespace to
# that list, with helm and no cluster:
#
#   kernel_network_policies.py chart     every port and webhook of the pods the
#                                        repository's own charts put there
#   kernel_network_policies.py census    everything else that is installed
#                                        there, by name and pinned version
#   kernel_network_policies.py switch    the operator is told the installer's
#                                        switch, and only "true" is on
#   kernel_network_policies.py upstream <release> <chart> <namespace> [helm args...]
#                                        the same check as `chart`, against a
#                                        chart given by path: for reading an
#                                        upstream chart again when its pin
#                                        moves. Not run by make.
# =============================================================================
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
INVENTORY = ROOT / "internal/kernel/kernelnet/inventory.yaml"
POD_KINDS = ("Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")
WEBHOOK_KINDS = ("ValidatingWebhookConfiguration", "MutatingWebhookConfiguration")


class Failure(Exception):
    pass


def helm(*args):
    run = subprocess.run(["helm", "template", *args], cwd=ROOT, capture_output=True, text=True)
    if run.returncode != 0:
        raise Failure(f"helm template {' '.join(args)}: {run.stderr.strip()}")
    return [d for d in yaml.safe_load_all(run.stdout) if isinstance(d, dict)]


def inventory():
    return yaml.safe_load(INVENTORY.read_text())


def kernel_namespaces():
    return [ns["name"] for ns in yaml.safe_load((ROOT / "kernel/namespaces.yaml").read_text())["kernel"]]


def bootstrap(*extra):
    """The bootstrap chart as B-01 renders it, with every phase switched on."""
    with tempfile.TemporaryDirectory() as tmp:
        layout = Path(tmp) / "namespaces.yaml"
        body = (ROOT / "kernel/namespaces.yaml").read_text()
        layout.write_text("namespaces:\n" + "".join("  " + line for line in body.splitlines(True)))
        return helm("boot", "kernel/bootstrap/chart", "-f", str(layout), "-f", "kernel/platforms.yaml",
                    "--set-string", "appsets.enabled=true", "--set-string", "operator.enabled=true",
                    "--set-string", "headlamp.oidc.enabled=true", "--set-string", "kernelDomain=k.example",
                    "--set-string", "cluster=c", "--set-string", "dnsProvider=cloudflare",
                    "--set-string", "versions.headlamp.chart=0.0.0",
                    "--set-string", "versions.headlamp.repo=https://example.invalid", *extra)


def pod_template(doc):
    spec = doc.get("spec", {})
    if doc["kind"] == "CronJob":
        spec = spec.get("jobTemplate", {}).get("spec", {})
    return spec.get("template") or {}


def workloads_of(docs, default_namespace):
    """(namespace, name, pod labels, {port name: number}) per pod set."""
    out = []
    for doc in docs:
        if doc.get("kind") not in POD_KINDS:
            continue
        template = pod_template(doc)
        ports = {}
        spec = template.get("spec") or {}
        for c in spec.get("containers", []) + spec.get("initContainers", []):
            for p in c.get("ports", []):
                ports[p.get("name") or str(p["containerPort"])] = p["containerPort"]
        out.append((doc["metadata"].get("namespace", default_namespace), doc["metadata"]["name"],
                    (template.get("metadata") or {}).get("labels") or {}, ports))
    return out


def entry_for(inv, namespace, labels):
    """The inventory workloads whose pod labels this pod carries."""
    for ns in inv["namespaces"]:
        if ns["name"] != namespace:
            continue
        return [w for w in ns["workloads"]
                if all(labels.get(k) == v for k, v in w["podLabels"].items())]
    return []


def state_of(entries, port):
    for w in entries:
        for p in w.get("ports", []):
            if p["port"] == port:
                for state in ("from", "open", "closed", "elsewhere"):
                    if p.get(state):
                        return state
    return None


def check_rendered(docs, default_namespace, only=None):
    """Every port of every kernel pod set, and every webhook, against the list."""
    inv = inventory()
    kernel = set(kernel_namespaces())
    failures = []
    sets = [w for w in workloads_of(docs, default_namespace) if w[0] in kernel and (only is None or w[0] in only)]
    if not sets:
        raise Failure("nothing rendered into a kernel namespace: the check would pass on an empty chart")
    for namespace, name, labels, ports in sets:
        entries = entry_for(inv, namespace, labels)
        if not entries and not ports:
            continue  # a pod that listens on nothing: a Job, which only calls
        if not entries:
            failures.append(f"{namespace}/{name}: no inventory workload has pod labels this pod carries ({labels})")
            continue
        for pname, number in sorted(ports.items(), key=lambda kv: kv[1]):
            if state_of(entries, number) is None:
                failures.append(f"{namespace}/{name}: port {number} ({pname}) is not in the inventory")
    # A Service's targetPort is a port somebody is meant to call.
    by_ns = {}
    for namespace, name, labels, ports in sets:
        by_ns.setdefault(namespace, []).append((name, labels, ports))
    services = {}
    for doc in docs:
        if doc.get("kind") != "Service":
            continue
        namespace = doc["metadata"].get("namespace", default_namespace)
        if namespace not in kernel:
            continue
        selector = doc["spec"].get("selector") or {}
        for sp in doc["spec"].get("ports", []):
            target = sp.get("targetPort", sp["port"])
            hit = False
            for name, labels, ports in by_ns.get(namespace, []):
                if not all(labels.get(k) == v for k, v in selector.items()):
                    continue
                number = ports.get(target) if isinstance(target, str) else target
                if number is None:
                    continue  # this pod set does not have the named port
                hit = True
                services[(namespace, doc["metadata"]["name"], sp["port"])] = (labels, number)
                state = state_of(entry_for(inv, namespace, labels), number)
                if state is None:
                    failures.append(f"{namespace}: Service {doc['metadata']['name']} port {sp['port']} reaches pod port {number} of {name}, which is not in the inventory")
            if not hit and selector:
                failures.append(f"{namespace}: Service {doc['metadata']['name']} port {sp['port']} selects no rendered pod with port {target}")
    # The API server is not a pod: a webhook's port has to admit any source.
    for doc in docs:
        if doc.get("kind") not in WEBHOOK_KINDS:
            continue
        for hook in doc.get("webhooks", []):
            svc = hook.get("clientConfig", {}).get("service")
            if not svc or svc.get("namespace", default_namespace) not in kernel:
                continue
            key = (svc.get("namespace", default_namespace), svc["name"], svc.get("port", 443))
            if key not in services:
                failures.append(f"webhook {hook['name']}: its Service {key} is not rendered")
                continue
            labels, number = services[key]
            state = state_of(entry_for(inv, key[0], labels), number)
            if state not in ("open", "elsewhere"):
                failures.append(f"webhook {hook['name']}: pod port {number} in {key[0]} is {state!r} in the inventory; the API server is no pod, so it must be open")
    if failures:
        raise Failure("\n  ".join(["not covered by internal/kernel/kernelnet/inventory.yaml:"] + failures))


def chart():
    docs = bootstrap()
    # The bootstrap chart's own pods (kube-oidc-proxy).
    check_rendered(docs, "kernel-control", only={"kernel-observability"})
    # The operator's chart, with the values its Application passes.
    app = next(d for d in docs if d["kind"] == "Application" and d["metadata"]["name"] == "gentian-os")
    source = next(s for s in app["spec"]["sources"] if s.get("path") == "charts/gentian-os")
    with tempfile.TemporaryDirectory() as tmp:
        values = Path(tmp) / "values.yaml"
        values.write_text(yaml.safe_dump(source["helm"]["valuesObject"]))
        rendered = helm("gentian-os", "charts/gentian-os", "-n", app["spec"]["destination"]["namespace"],
                        "-f", str(values), "--set", "webhook.enabled=true", "--set", "custodian.enabled=true")
    names = {(d["metadata"].get("namespace"), d["metadata"]["name"]) for d in rendered if d.get("kind") == "Deployment"}
    for want in ("gentian-os-director", "gentian-os-usher", "gentian-os-custodian", "gentian-os-registrar", "gentian-os-bouncer"):
        if not any(name == want for _, name in names):
            raise Failure(f"the operator's chart rendered no {want}: the check would not have looked at it")
    check_rendered(rendered, app["spec"]["destination"]["namespace"])


# Applications of the bootstrap chart that put no pod into a kernel namespace.
NO_PODS = {
    "kyverno-policies": "ClusterPolicy objects only",
    "gentian-appsets": "ApplicationSet objects only",
}


def census():
    inv = inventory()
    kernel = set(kernel_namespaces())
    installed = {}
    for ns in inv["namespaces"]:
        for w in ns["workloads"]:
            installed.setdefault(w["installedBy"], []).append((ns["name"], w))
    failures = []

    def pinned(installer, namespace, version, what):
        entries = [w for n, w in installed.get(installer, []) if n == namespace]
        if not entries:
            failures.append(f"{what} installs into {namespace} and no inventory workload says installedBy: {installer}")
            return
        if version and not any(str(version) in w["readAt"] for w in entries):
            failures.append(f"{what} is at {version}; the inventory read it at {sorted({w['readAt'] for w in entries})}. Read its pods, ports and webhooks again at that version, then update readAt")

    # Argo CD Applications of the bootstrap chart.
    for doc in bootstrap():
        if doc["kind"] != "Application":
            continue
        name, namespace = doc["metadata"]["name"], doc["spec"]["destination"].get("namespace")
        if namespace not in kernel or name in NO_PODS:
            continue
        source = doc["spec"].get("source") or doc["spec"]["sources"][0]
        version = source.get("targetRevision") if source.get("chart") and name != "headlamp" else None
        pinned(f"bootstrap-application:{name}", namespace, version, f"Application {name}")

    # What the installer's own steps install with helm or a manifest.
    versions = yaml.safe_load((ROOT / "versions.yaml").read_text())
    functions = {ns["function"]: ns["name"] for ns in yaml.safe_load((ROOT / "kernel/namespaces.yaml").read_text())["kernel"]}
    seen = 0
    for step in sorted((ROOT / "scripts/steps").glob("*.sh")):
        text = step.read_text()
        step_id = re.search(r"^# step: (\S+)", text, re.M).group(1)
        for release, fn in re.findall(r"^\s*helm_pinned (\S+) \S+ (\S+)", text, re.M):
            seen += 1
            pinned(f"step:{step_id}", functions[fn], versions[release]["chart"], f"{step_id} (helm release {release})")
    if seen < 4:
        raise Failure(f"found {seen} helm_pinned installs in scripts/steps; the pattern this reads them by no longer matches")
    argocd = (ROOT / "scripts/steps/A-06-argocd.sh").read_text()
    if "manifests/install.yaml" not in argocd or "argocd-image-updater" not in argocd:
        raise Failure("A-06 no longer installs Argo CD and its image updater the way this reads")
    pinned("step:A-06-argocd", functions["gitops"], versions["argocd"]["manifest"], "A-06 (Argo CD manifest)")

    # The identity provider and OpenFGA, which the Suze composition installs.
    suze = (ROOT / "crossplane/compositions/suze.yaml").read_text()
    for var, namespace in (("kcVersion", "kernel-authentication"), ("fgaVersion", "kernel-authorization")):
        m = re.search(r'\$' + var + r' := default "([^"]+)"', suze)
        if not m:
            raise Failure(f"suze.yaml no longer sets ${var} the way this reads")
        pinned("composition:suze", namespace, m.group(1), f"the Suze composition ({var})")

    # Crossplane's providers and functions.
    for doc in yaml.safe_load_all((ROOT / "crossplane/providers/providers.yaml").read_text()):
        if isinstance(doc, dict) and doc.get("kind") in ("Provider", "Function"):
            pinned("step:B-05-crossplane-providers", "kernel-provisioning", None, f"{doc['kind']} {doc['metadata']['name']}")

    # Every installedBy names something this looked at, or says in words what
    # creates the pods (cert-manager's solver).
    known = ("bootstrap-application:", "bootstrap-chart:", "step:", "composition:")
    for installer in installed:
        if installer.startswith(known):
            continue
        if " " not in installer:
            failures.append(f"installedBy: {installer} is neither a known installer nor a sentence")
    if failures:
        raise Failure("\n  ".join(["the inventory and what is installed disagree:"] + failures))


def switch():
    """The operator's KERNEL_NETWORK_POLICIES follows the installer's, and only "true" is on."""
    def operator_env(*extra):
        docs = bootstrap(*extra)
        app = next(d for d in docs if d["kind"] == "Application" and d["metadata"]["name"] == "gentian-os")
        source = next(s for s in app["spec"]["sources"] if s.get("path") == "charts/gentian-os")
        passed = source["helm"]["valuesObject"].get("kernelNetworkPolicies")
        with tempfile.TemporaryDirectory() as tmp:
            values = Path(tmp) / "values.yaml"
            values.write_text(yaml.safe_dump(source["helm"]["valuesObject"]))
            rendered = helm("gentian-os", "charts/gentian-os", "-n", "kernel-control", "-f", str(values))
        for doc in rendered:
            if doc.get("kind") != "Deployment" or doc["metadata"]["name"] != "gentian-os":
                continue
            for env in doc["spec"]["template"]["spec"]["containers"][0]["env"]:
                if env["name"] == "KERNEL_NETWORK_POLICIES":
                    return passed, env.get("value")
        raise Failure("the operator's Deployment has no KERNEL_NETWORK_POLICIES")

    for extra, want in (((), "false"),
                        (("--set-string", "kernelNetworkPolicies=true"), "true"),
                        (("--set-string", "kernelNetworkPolicies=false"), "false"),
                        (("--set", "kernelNetworkPolicies=true"), "true"),
                        (("--set-string", "kernelNetworkPolicies=yes"), "false")):
        passed, got = operator_env(*extra)
        if passed != want or got != want:
            raise Failure(f"bootstrap chart {' '.join(extra) or '(default)'}: the operator's chart is passed {passed!r} and the operator told {got!r}, want {want!r}")
    b01 = (ROOT / "scripts/steps/B-01-bootstrap-apps.sh").read_text()
    if '"kernelNetworkPolicies=${KERNEL_NETWORK_POLICIES:-false}"' not in b01:
        raise Failure("B-01 no longer passes KERNEL_NETWORK_POLICIES to the bootstrap chart, off by default")


def upstream(release, chart_path, namespace, *extra):
    docs = helm(release, chart_path, "-n", namespace, *extra)
    check_rendered(docs, namespace)


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__ or "usage: kernel_network_policies.py chart|census|upstream ...")
    try:
        {"chart": chart, "census": census, "switch": switch, "upstream": upstream}[sys.argv[1]](*sys.argv[2:])
    except Failure as e:
        print(f"    {e}")
        sys.exit(1)


if __name__ == "__main__":
    main()
