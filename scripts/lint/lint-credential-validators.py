#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-credential-validators.py — a generated requirement must be
# one its own CRD will admit
# =============================================================================
# The catalogue is generated from credentials.yaml and kernel/platforms.yaml,
# and the CRD that admits it is generated from api/v1alpha1. Both generators
# were correct and disagreed with each other: platforms.yaml declared
# `validate: cloudflare-tunnel`, the CRD's enum did not carry that value, and
# every check passed. make verify-gen compares each generated file to its
# source, not to the other; the Go tests never apply a CR; CI has no cluster.
#
# So the failure surfaced at `kubectl apply` on a real install, at step C-06,
# with the install aborting half-configured:
#
#   The CredentialRequirement "edge-ingress-cf-tunnel" is invalid:
#   spec.validate.type: Unsupported value: "cloudflare-tunnel"
#
# This reads the enum out of the CRD and the values out of the catalogue and
# asserts the second is a subset of the first — the comparison neither
# generator can make alone.
#
# Usage: scripts/lint/lint-credential-validators.py
# =============================================================================

import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
CRD = ROOT / "charts" / "gentian-os" / "crds" / "gentianos.io_credentialrequirements.yaml"
CATALOGUE = ROOT / "kernel" / "credentials" / "credential-requirements.yaml"

GREEN, RED, NC = "\033[0;32m", "\033[0;31m", "\033[0m"


def crd_enum() -> set[str]:
    """The values spec.validate.type will admit, from the CRD itself."""
    doc = yaml.safe_load(CRD.read_text())
    for version in doc["spec"]["versions"]:
        props = version["schema"]["openAPIV3Schema"]["properties"]
        validate = props["spec"]["properties"].get("validate")
        if not validate:
            continue
        return set(validate["properties"]["type"].get("enum") or [])
    return set()


def catalogue_types() -> list[tuple[str, str]]:
    """(requirement name, validator type) for everything the generator emits."""
    out = []
    for doc in yaml.safe_load_all(CATALOGUE.read_text()):
        if not doc or doc.get("kind") != "CredentialRequirement":
            continue
        name = (doc.get("metadata") or {}).get("name", "<unnamed>")
        vtype = ((doc.get("spec") or {}).get("validate") or {}).get("type")
        if vtype:
            out.append((name, vtype))
    return out


def main() -> int:
    allowed = crd_enum()
    if not allowed:
        print(f"{RED}Could not read the validator enum from {CRD}.{NC}")
        return 1

    bad = [(n, t) for n, t in catalogue_types() if t not in allowed]
    if bad:
        print(f"{RED}A generated requirement names a validator its CRD will refuse:{NC}")
        for name, vtype in bad:
            print(f"  {name}: validate.type={vtype!r}")
        print()
        print("  The catalogue and the CRD are generated from different sources —")
        print("  kernel/platforms.yaml or credentials.yaml, and api/v1alpha1 — so")
        print("  each can be internally correct while disagreeing with the other.")
        print(f"  Add the value to the +kubebuilder:validation:Enum on")
        print("  CredentialValidation.Type and run 'make manifests'.")
        print(f"  Admitted today: {', '.join(sorted(allowed))}")
        return 1

    checked = len(catalogue_types())
    print(f"{GREEN}Every generated validator is one the CRD admits{NC} "
          f"({checked} requirement(s), {len(allowed)} permitted values).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
