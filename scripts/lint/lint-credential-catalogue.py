#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-credential-catalogue.py — a generated requirement must be
# reachable from the installer
# =============================================================================
# The catalogue has two readers. scripts/gen/gen-credential-requirements.py
# renders every provider table into the on-cluster CredentialRequirement set;
# scripts/lib/credentials.sh walks the same tables to decide what to prompt
# for. Each was internally correct and they disagreed about what exists:
# edgeIngress was generated, and the installer — which knew about exactly one
# hard-coded table — could not see it. So the operator was never asked for the
# tunnel token, nothing was seeded to its vault path, and nothing failed. The
# generator passed, the Go tests passed, and the only symptom was a prompt that
# did not happen.
#
# This compares the two lists. It is the check neither side can do alone, and
# the only kind that catches a requirement nobody asks for: every artefact is
# individually valid, and the defect lives in the gap between them.
#
# Usage: scripts/lint/lint-credential-catalogue.py
# =============================================================================

import re
import subprocess
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
GEN = ROOT / "scripts" / "gen" / "gen-credential-requirements.py"
SHELL = ROOT / "scripts" / "lib" / "credentials.sh"
PLATFORMS = ROOT / "kernel" / "platforms.yaml"

GREEN, RED, NC = "\033[0;32m", "\033[0;31m", "\033[0m"


def generator_tables() -> set[tuple[str, str]]:
    """PROVIDER_TABLES as the generator declares them."""
    src = GEN.read_text()
    block = re.search(r"PROVIDER_TABLES\s*=\s*\((.*?)\)\s*\n\n", src, re.S)
    if not block:
        return set()
    return set(re.findall(r'\("([^"]+)",\s*"([^"]+)"\)', block.group(1)))


def shell_tables() -> set[tuple[str, str]]:
    """GENTIAN_PROVIDER_TABLES as the installer declares them."""
    src = SHELL.read_text()
    block = re.search(r"GENTIAN_PROVIDER_TABLES=\((.*?)\n\)", src, re.S)
    if not block:
        return set()
    out = set()
    for row in re.findall(r'"([^"]+)"', block.group(1)):
        parts = row.split(":")
        if len(parts) >= 2:
            out.add((parts[0], parts[1]))
    return out


def credentialled_providers() -> set[str]:
    """Requirement names implied by platforms.yaml, using the shared rule."""
    platforms = yaml.safe_load(PLATFORMS.read_text())
    names = set()
    for table, prefix in generator_tables():
        for provider, profile in (platforms.get(table) or {}).items():
            if (profile or {}).get("credential"):
                names.add(f"{prefix}-{provider}")
    return names


def shell_reachable(names: set[str]) -> set[str]:
    """Which of those the installer can address FOR A CLUSTER THAT SELECTS IT.

    Each provider is tested with the cluster configured to use it, because
    _provider_req_path resolves only the active provider of each table — that
    is the whole point of it, so asking without selecting would report every
    provider but one as unreachable.

    Every selector variable is set to the provider under test rather than a
    per-table mapping: a table whose provider does not match simply finds no
    credential and is skipped, so this needs no list of which variable drives
    which table, and a new table needs no change here.
    """
    prefixes = {p for _, p in generator_tables()}
    reachable = set()
    for name in sorted(names):
        provider = next((name[len(p) + 1:] for p in prefixes if name.startswith(p + "-")), "")
        if not provider:
            continue
        script = f"""
            set -uo pipefail
            SCRIPT_DIR={ROOT}
            export DNS_PROVIDER={provider} EDGE_INGRESS={provider}
            yq_get() {{ yq -r "$1" "$2" 2>/dev/null; }}
            source {SHELL} 2>/dev/null || true
            _provider_req_path {name} >/dev/null 2>&1 && echo OK
        """
        try:
            r = subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=60)
            if "OK" in r.stdout:
                reachable.add(name)
        except Exception:
            pass
    return reachable


def main() -> int:
    gen, sh = generator_tables(), shell_tables()
    if not gen or not sh:
        print(f"{RED}Could not read the provider tables from both sides.{NC}")
        print(f"  generator: {sorted(gen)}")
        print(f"  installer: {sorted(sh)}")
        return 1

    if gen != sh:
        print(f"{RED}The generator and the installer disagree about which provider tables exist:{NC}")
        for t in sorted(gen - sh):
            print(f"  generated but the installer cannot see it: {t}")
        for t in sorted(sh - gen):
            print(f"  the installer expects it but nothing generates it: {t}")
        print()
        print("  A table only the generator knows produces a requirement with a vault")
        print("  path that nothing ever writes to, and a prompt that never happens.")
        print("  Add the row to GENTIAN_PROVIDER_TABLES in scripts/lib/credentials.sh")
        print("  or to PROVIDER_TABLES in scripts/gen/gen-credential-requirements.py.")
        return 1

    # The tables agree; now check a cluster selecting each provider can actually
    # address its requirement.
    names = credentialled_providers()
    unreachable = names - shell_reachable(names)
    if unreachable:
        print(f"{RED}Requirements the installer cannot address for any cluster:{NC}")
        for n in sorted(unreachable):
            print(f"  {n}")
        print()
        print("  Each is generated into kernel/credentials/credential-requirements.yaml")
        print("  and would never be prompted for or seeded.")
        return 1

    print(f"{GREEN}Provider tables agree and every generated requirement is reachable{NC} "
          f"({len(sh)} table(s), {len(names)} provider credential(s)).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
