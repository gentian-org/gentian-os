#!/usr/bin/env python3
# =============================================================================
# scripts/tests/mail_listeners.py — the assertions behind test-mail-listeners.sh.
#
# Renders, with helm and no cluster, the mail ApplicationSet as the wrapper
# writes it and the mail edge's chart with the parameters Argo CD passes it,
# and reports which ports the edge's load balancer serves.
#
#   mail_listeners.py ports [--set-string k=v ...]   the ports, one line
# =============================================================================
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]


class Failure(Exception):
    pass


def helm(*args):
    run = subprocess.run(["helm", "template", *args], cwd=ROOT, capture_output=True, text=True)
    if run.returncode != 0:
        raise Failure(f"helm template: {run.stderr.strip()}")
    return [d for d in yaml.safe_load_all(run.stdout) if isinstance(d, dict)]


def appsets(*extra):
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        f.write("namespaces:\n")
        for line in (ROOT / "kernel/namespaces.yaml").read_text().splitlines():
            f.write("  " + line + "\n")
        f.write("versions:\n")
        for line in (ROOT / "versions.yaml").read_text().splitlines():
            f.write("  " + line + "\n")
        f.flush()
        return helm("appsets", "kernel/appsets", "-f", f.name, "--set", "mailServiceMode=system", *extra)


def edge_ports(*extra):
    sets = [d for d in appsets(*extra) if d.get("kind") == "ApplicationSet" and d["metadata"]["name"] == "gentian-mail"]
    if len(sets) != 1:
        raise Failure("no gentian-mail ApplicationSet rendered")
    spec = sets[0]["spec"]
    stage, edge = None, None
    for generator in spec["generators"][0]["matrix"]["generators"]:
        for item in generator["list"]["elements"]:
            if "env" in item:
                stage = item["env"]
            elif item.get("app") == "mail-edge":
                edge = item
    if edge is None:
        raise Failure("the mail ApplicationSet deploys no mail-edge")
    args = ["edge", "kernel/services/mail-edge/manifests", "-n", edge["namespace"]]
    for p in spec["template"]["spec"]["source"]["helm"]["parameters"]:
        value = p["value"].replace("{{.namespace}}", edge["namespace"]).replace("{{.env}}", stage)
        args += ["--set-string" if p.get("forceString") else "--set", f"{p['name']}={value}"]
    services = [d for d in helm(*args) if d.get("kind") == "Service" and d["spec"].get("type") == "LoadBalancer"]
    if not services:
        return []
    return sorted(port["port"] for port in services[0]["spec"]["ports"])


def main(argv):
    if not argv or argv[0] != "ports":
        print(__doc__ or "usage: mail_listeners.py ports [helm args]", file=sys.stderr)
        return 2
    try:
        print(" ".join(str(p) for p in edge_ports(*argv[1:])))
    except Failure as e:
        print(f"FAIL {e}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
