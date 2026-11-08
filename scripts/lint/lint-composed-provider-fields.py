#!/usr/bin/env python3
"""Every composed managed resource names only fields its provider declares.

A field the pinned provider's schema does not declare is refused when the
composite applies the resource ("field not declared in schema"), and the
whole composite stops syncing: nothing after it is created. Neither
`crossplane render` nor the golden files notice, because rendering does not
validate against the providers' CRDs.

So this reads what each pinned provider package declares under
spec.forProvider and spec.initProvider (crossplane/providers/types/*.yaml,
written by scripts/gen/gen-provider-types.py) and holds every rendered
resource of crossplane/tests/unit/render/*/expected.yaml to it. Only the
top-level keys of the two blocks are compared: that is where a field is
added or renamed between provider versions.

Usage:
    python3 scripts/lint/lint-composed-provider-fields.py
"""

import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit("PyYAML is required: pip install pyyaml")

ROOT = Path(__file__).resolve().parents[2]
TYPES = ROOT / "crossplane" / "providers" / "types"
RENDERS = ROOT / "crossplane" / "tests" / "unit" / "render"
BLOCKS = ("forProvider", "initProvider")


def declared() -> dict[tuple[str, str], set[str]]:
    """(group, kind) -> the top-level fields of forProvider."""
    out: dict[tuple[str, str], set[str]] = {}
    for path in sorted(TYPES.glob("*.yaml")):
        doc = yaml.safe_load(path.read_text()) or {}
        for t in doc.get("types") or []:
            if "forProvider" not in t:
                continue
            group = t["name"].split(".", 1)[1]
            out[(group, t["kind"])] = set(t["forProvider"])
    return out


def main() -> int:
    known = declared()
    if not known:
        print("lint-composed-provider-fields: no provider declares forProvider fields; "
              "run `make refresh-provider-types`", file=sys.stderr)
        return 1
    errors = []
    checked = 0
    for expected in sorted(RENDERS.glob("*/expected.yaml")):
        for doc in yaml.safe_load_all(expected.read_text()):
            if not isinstance(doc, dict):
                continue
            group = str(doc.get("apiVersion", "")).split("/", 1)[0]
            fields = known.get((group, doc.get("kind")))
            if fields is None:
                continue
            spec = doc.get("spec") or {}
            for block in BLOCKS:
                given = spec.get(block)
                if not isinstance(given, dict):
                    continue
                checked += 1
                for key in sorted(set(given) - fields):
                    name = (doc.get("metadata") or {}).get("name", "?")
                    errors.append(
                        f"{expected.relative_to(ROOT)}: {doc['kind']} {name}: "
                        f"spec.{block}.{key} is not declared by the pinned provider"
                    )
    if errors:
        print("Composed resources name fields their provider does not declare:", file=sys.stderr)
        for e in errors:
            print(f"  {e}", file=sys.stderr)
        return 1
    print(f"Every composed resource names declared provider fields ({checked} blocks checked).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
