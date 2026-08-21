#!/usr/bin/env python3
"""Render provider-kubernetes-kinds.yaml into the ClusterRole in provider-rbac.yaml.

The ClusterRole is generated, not hand-maintained, for the reason the kind list's
own header explains: the kind set has to be a union of measured sources, and a
hand-edit is exactly the drift this exists to prevent. Edit
crossplane/providers/provider-kubernetes-kinds.yaml and re-run `make gen-all`;
never edit the rules block between the markers directly.

Usage:
    python3 scripts/gen/gen-provider-rbac.py
    python3 scripts/gen/gen-provider-rbac.py --check   # CI: fails if stale
"""

import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit("PyYAML is required: pip install pyyaml")

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "crossplane" / "providers" / "provider-kubernetes-kinds.yaml"
TARGET = ROOT / "crossplane" / "providers" / "provider-rbac.yaml"

BEGIN = "# BEGIN generated rules — from provider-kubernetes-kinds.yaml, do not edit\n"
END = "# END generated rules\n"


def render_rules(data: dict) -> str:
    verbs = data["verbs"]
    lines = []
    for group in data["kinds"]:
        api_group = group["group"]
        lines.append(f"  - apiGroups: [{api_group!r}]")
        resources = ", ".join(repr(r) for r in group["resources"])
        lines.append(f"    resources: [{resources}]")
        verb_list = ", ".join(repr(v) for v in verbs)
        lines.append(f"    verbs: [{verb_list}]")
    return "\n".join(lines) + "\n"


def main() -> int:
    check = "--check" in sys.argv
    data = yaml.safe_load(SOURCE.read_text())
    rules = render_rules(data)

    text = TARGET.read_text()
    if BEGIN not in text or END not in text:
        sys.exit(f"FATAL: {TARGET.name} has no generated-rules markers.")
    head, rest = text.split(BEGIN, 1)
    _, tail = rest.split(END, 1)
    updated = f"{head}{BEGIN}{rules}{END}{tail}"

    n_kinds = sum(len(g["resources"]) for g in data["kinds"])
    if updated == text:
        print(f"provider-kubernetes ClusterRole matches provider-kubernetes-kinds.yaml "
              f"({n_kinds} resources).")
        return 0
    if check:
        print(
            "ERROR: the generated ClusterRole is stale.\n"
            "       Run: make gen-provider-rbac",
            file=sys.stderr,
        )
        return 1
    TARGET.write_text(updated)
    print(f"wrote {TARGET.relative_to(ROOT)} ({n_kinds} resources)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
