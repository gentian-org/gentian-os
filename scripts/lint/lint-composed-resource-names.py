#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-composed-resource-names.py — names Kubernetes will refuse
# =============================================================================
# Composed resources are named from the thing they represent, and Keycloak names
# things in ways Kubernetes will not accept. `gentian_useruuid` has an
# underscore. `VERIFY_PROFILE` lowercases to `verify_profile`. Some catalogues
# name a mapper "full name", with a space.
#
# An object name must be an RFC 1123 subdomain, so each of those produces:
#
#   cannot apply composed resource "corp-...-scope-mapper-gentian_useruuid"
#   because it has an invalid name. Must be a valid RFC 1123 subdomain name
#
# and the composite sits Synced=False until someone reads the message. That has
# happened three times: the OIDC pack's protocol mappers, then again while they
# were being moved, then the realm's required actions.
#
# `crossplane render` does not catch it. It renders the manifest and stops there
# — no API server sees the object, so nothing validates the name. The render
# fixtures are therefore green with a name the cluster will refuse, which is the
# worst place for this to be silent.
#
# The rule to write templates by: the external-name and the forProvider field
# keep whatever the upstream system calls the thing, because that is what
# identifies it. Only the Kubernetes object name is slugged.
#
# Usage:
#   python3 scripts/lint/lint-composed-resource-names.py
# =============================================================================
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
RENDER_DIR = ROOT / "crossplane/tests/unit/render"

RED = "\033[0;31m"
GREEN = "\033[0;32m"
YELLOW = "\033[0;33m"
DIM = "\033[2m"
NC = "\033[0m"

# RFC 1123 subdomain, which is what a Kubernetes object name must be.
RFC1123 = re.compile(r"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")
MAX_LEN = 253


def main() -> int:
    try:
        import yaml
    except ImportError:
        print(f"{YELLOW}SKIP{NC} — PyYAML not installed.")
        return 0

    if not RENDER_DIR.is_dir():
        print(f"{YELLOW}SKIP{NC} — no render fixtures at {RENDER_DIR.relative_to(ROOT)}.")
        return 0

    bad, checked = [], 0
    for expected in sorted(RENDER_DIR.glob("*/expected.yaml")):
        case = expected.parent.name
        try:
            docs = list(yaml.safe_load_all(expected.read_text()))
        except yaml.YAMLError as exc:
            print(f"  {RED}ERROR{NC} {case}: cannot parse expected.yaml — {exc}")
            bad.append((case, "<unparseable>", "invalid YAML"))
            continue
        for doc in docs:
            if not isinstance(doc, dict):
                continue
            name = (doc.get("metadata") or {}).get("name")
            if not name:
                continue
            checked += 1
            if not RFC1123.match(name):
                bad.append((case, name, f"{doc.get('kind', '?')} is not an RFC 1123 subdomain"))
            elif len(name) > MAX_LEN:
                bad.append((case, name, f"{doc.get('kind', '?')} exceeds {MAX_LEN} characters"))

    print("")
    print("Composed resource names, as Kubernetes would accept them")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    for case, name, why in bad:
        print(f"  {RED}ERROR{NC} {case}: {name}")
        print(f"        {DIM}{why}{NC}")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    if bad:
        print(f"{RED}{len(bad)} composed resource name(s) the cluster will refuse.{NC}")
        print(f"{DIM}Slug the object name; leave external-name and forProvider as the{NC}")
        print(f"{DIM}upstream system spells it, because that is what identifies it.{NC}")
        return 1
    print(f"{GREEN}Every composed resource name is one Kubernetes will accept{NC} "
          f"({checked} checked).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
