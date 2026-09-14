#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-plan-defaults.py — does a scaffolded tenant land on a plan?
# =============================================================================
# Two files state the same quotas and only one of them is priced.
#
# usage.plans.catalogue.base in charts/gentian-os/values.yaml is the default
# plan: a ceiling with a SKU behind it, which the resources API will accept and
# a month of usage resolves against. The tenant-defaults component gtnctl
# scaffolds carries the same numbers so that a freshly deployed tenant starts
# ON that plan rather than on a ceiling matching none.
#
# resourceplans.yaml says they are "deliberately identical". Nothing checked.
# Identical-by-comment is identical until someone edits one of them, and the
# consequence is not a failure: the tenant provisions, runs, and reports a
# custom ceiling that no plan matches and no SKU bills. It is wrong quietly, in
# the direction of money.
#
# The node unit was rescaled from 2 vCPU / 4 GB to 4 CPU / 16 GB in one of the
# two files first, which is exactly the window this exists to close.
#
# storage and maxApps are deliberately NOT compared. maxApps is a policy limit
# the scaffold sets and no plan carries -- resourceplans.yaml says so -- and
# storage is per-plan in the catalogue while the scaffold pins the base figure.
# Comparing them would report a difference that is meant to be there.
# =============================================================================

import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    print("PyYAML not available; skipping plan defaults lint.")
    sys.exit(0)

ROOT = Path(__file__).resolve().parents[2]
VALUES = ROOT / "charts" / "gentian-os" / "values.yaml"
SCAFFOLD = ROOT / "scripts" / "kubectl-gentian"

# The fields that are sold or that bound a tenant. storage/maxApps excluded --
# see the header.
COMPARED = ("requestsCpu", "requestsMemory", "cpu", "memory")

GREEN, RED, DIM, RESET = "\033[0;32m", "\033[0;31m", "\033[2m", "\033[0m"


def base_plan_quotas():
    doc = yaml.safe_load(VALUES.read_text())
    return (((doc.get("usage") or {}).get("plans") or {})
            .get("catalogue", {}).get("base", {}).get("quotas"))


def scaffold_quotas():
    """The quotas: block from the generator's heredoc.

    Read as text rather than by running the generator: it needs a cluster, and
    what is asserted here is what it would WRITE.
    """
    text = SCAFFOLD.read_text()
    match = re.search(r"^  quotas:\n((?:^(?:    .*|\s*)\n)+)", text, re.M)
    if not match:
        return None
    try:
        return (yaml.safe_load("quotas:\n" + match.group(1)) or {}).get("quotas")
    except yaml.YAMLError:
        return None


def main():
    print("Plan defaults — does a scaffolded tenant land on the base plan?")
    print(f"{DIM}{'─' * 60}{RESET}")

    plan, scaffold = base_plan_quotas(), scaffold_quotas()
    if plan is None:
        print(f"  {RED}ERROR{RESET}  no usage.plans.catalogue.base.quotas in {VALUES.name}")
        return 1
    if scaffold is None:
        print(f"  {RED}ERROR{RESET}  could not read the quotas block from {SCAFFOLD.name}")
        print("         The generator does write one, so this means the extraction")
        print("         stopped matching — passing here would check nothing.")
        return 1

    failures = []
    for field in COMPARED:
        want, got = plan.get(field), scaffold.get(field)
        if str(want) != str(got):
            failures.append((field, want, got))

    for field, want, got in failures:
        print(f"  {RED}ERROR{RESET}  {field}: base plan says {want!r}, scaffold writes {got!r}")

    print(f"{DIM}{'─' * 60}{RESET}")
    if failures:
        print(f"{RED}{len(failures)} quota(s) differ between the base plan and the scaffold.{RESET}")
        print("  A tenant scaffolded from these lands on a ceiling no plan matches,")
        print("  so it reports a custom quota and bills against no SKU.")
        return 1
    print(f"{GREEN}The scaffolded tenant lands exactly on the base plan{RESET} "
          f"({len(COMPARED)} quota(s) compared).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
