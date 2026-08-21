#!/usr/bin/env python3
"""Does every kind a render fixture composes appear in provider-kubernetes-kinds.yaml?

provider-kubernetes-kinds.yaml is a committed union of measured sources — see its
own header for why it has to be a union rather than one source. This is the half
of that contract nothing else checks: a Composition can grow a new `kind: Object`
manifest kind, add a fixture that renders it, and the ClusterRole stays exactly
as it was. `crossplane render` does not enforce RBAC, so the fixture passes and
the gap is invisible until it reaches a real cluster and provider-kubernetes is
refused.

This walks every render fixture's expected.yaml, collects the kinds inside
`kind: Object` -> spec.forProvider.manifest (the shape provider-kubernetes
manages), and fails when one is not in the committed list.

It cannot find a kind that appears in NEITHER a fixture nor a live cluster —
that gap is closed by re-measuring against a cluster that has exercised the
path, not by this script. See provider-kubernetes-kinds.yaml.

Usage:
    scripts/lint/lint-provider-rbac.py
"""

import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit("PyYAML is required: pip install pyyaml")

ROOT = Path(__file__).resolve().parents[2]
KINDS_FILE = ROOT / "crossplane" / "providers" / "provider-kubernetes-kinds.yaml"
FIXTURES = sorted((ROOT / "crossplane" / "tests" / "unit" / "render").glob("*/expected.yaml"))


def fixture_kinds() -> dict[tuple[str, str], list[str]]:
    """{(group, kind): [fixture names it appears in]}"""
    out: dict[tuple[str, str], list[str]] = {}
    for f in FIXTURES:
        case = f.parent.name
        for doc in yaml.safe_load_all(f.read_text()):
            if not isinstance(doc, dict):
                continue
            forprovider = (doc.get("spec") or {}).get("forProvider", {})
            manifest = forprovider.get("manifest") if isinstance(forprovider, dict) else None
            if not isinstance(manifest, dict):
                continue
            av, kind = manifest.get("apiVersion"), manifest.get("kind")
            if not av or not kind:
                continue
            group = av.split("/")[0] if "/" in av else ""
            out.setdefault((group, kind), []).append(case)
    return out


def kind_to_group_resource_pairs() -> dict[tuple[str, str], set[str]]:
    """(group, kind) -> resources, using the kind list's own inline comments
    ("- configmaps  # ConfigMap") to recover Kind names without a second
    hardcoded mapping to keep in sync."""
    import re
    text = KINDS_FILE.read_text()
    data = yaml.safe_load(text)
    pairs: dict[tuple[str, str], set[str]] = {}
    for group in data["kinds"]:
        g = group["group"]
        for line in text.splitlines():
            m = re.match(r"\s*-\s+(\S+)\s+#\s+(\S+)\s*$", line)
            if not m:
                continue
            resource, kind = m.group(1), m.group(2)
            if resource in group["resources"]:
                pairs.setdefault((g, kind), set()).add(resource)
    return pairs


def main() -> int:
    covered = kind_to_group_resource_pairs()
    by_kind = fixture_kinds()
    if not by_kind:
        sys.exit("FATAL: no render fixtures produced any kind:Object manifest — has the layout changed?")

    missing = sorted(set(by_kind) - set(covered))
    for group, kind in missing:
        cases = ", ".join(by_kind[(group, kind)])
        print(
            f"ERROR: {group or '(core)'}/{kind} is composed in fixture(s) [{cases}] "
            f"but is not in {KINDS_FILE.relative_to(ROOT)}.\n"
            f"       provider-kubernetes will be refused when it reaches a real cluster.",
            file=sys.stderr,
        )
    if missing:
        print(f"\n{len(missing)} kind(s) composed but not covered by the RBAC kind list.",
              file=sys.stderr)
        return 1
    print(f"Every kind a render fixture composes is in provider-kubernetes-kinds.yaml "
          f"({len(by_kind)} kinds checked).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
