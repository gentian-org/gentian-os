#!/usr/bin/env python3
"""Copy the Envoy Gateway CRDs this repository writes objects of into the operator's tests.

The operator builds SecurityPolicy, BackendTrafficPolicy and ClientTrafficPolicy
objects as untyped maps, and the installer renders an EnvoyProxy from a chart.
Nothing compiles against Envoy Gateway's types, so a misspelt field is found by
the install and not by the build. The operator's tests therefore create every
such object against the definitions of the pinned release, with unknown fields
refused (internal/controller/envoy_gateway_schema_test.go), and this script is
where those definitions come from.

It reads the chart version from versions.yaml, asks helm for the CRDs that
chart installs, and keeps the four kinds this repository writes. Run it when the
envoy-gateway pin changes; a test fails until it has been.

Usage:
    python3 scripts/gen/gen-envoy-gateway-crds.py
"""

import re
import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit("PyYAML is required: pip install pyyaml")

ROOT = Path(__file__).resolve().parents[2]
TARGET = ROOT / "internal" / "controller" / "testdata" / "envoy-gateway-crds"
KINDS = ("backendtrafficpolicies", "clienttrafficpolicies", "envoyproxies", "securitypolicies")


def main() -> int:
    pin = yaml.safe_load((ROOT / "versions.yaml").read_text())["envoy-gateway"]
    version, repo = pin["chart"], pin["repo"]
    shown = subprocess.run(
        ["helm", "show", "crds", repo, "--version", version],
        check=True, capture_output=True, text=True,
    ).stdout
    TARGET.mkdir(parents=True, exist_ok=True)
    for old in TARGET.glob("*.yaml"):
        old.unlink()
    written = set()
    for doc in re.split(r"^---\s*$", shown, flags=re.M):
        parsed = yaml.safe_load(doc)
        if not parsed:  # a separator followed by nothing but comments
            continue
        name = parsed["metadata"]["name"]
        plural, _, group = name.partition(".")
        if group != "gateway.envoyproxy.io" or plural not in KINDS:
            continue
        # The text as the chart has it, not a re-serialisation of it.
        (TARGET / f"{group}_{plural}.yaml").write_text("---\n" + doc.strip("\n") + "\n")
        written.add(plural)
    missing = sorted(set(KINDS) - written)
    if missing:
        print(f"chart {version} carries no CRD for: {', '.join(missing)}", file=sys.stderr)
        return 1
    (TARGET / "VERSION").write_text(version + "\n")
    print(f"envoy-gateway {version}: {len(written)} CRDs written to {TARGET.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
