#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-step-order.py - does every step's `requires:` run earlier?
# =============================================================================
# Each step declares what it needs:
#
#     # step: B-08-seed-secrets
#     # requires: C-01-cluster-claim
#
# Nothing checked that the named step runs BEFORE it. B-08 declared C-01,
# which runs two steps later, and the contract was read by a person as an
# ordering statement while the driver treated it as a comment. The install
# works, so nothing failed; the documentation was simply false, and the next
# person to reorder the steps would have believed it.
#
# Steps run in the lexical order of their filenames, which is what the phase
# letter and number are for. So the check is exactly: the required step exists
# in the same set, and its filename sorts earlier.
#
# A step may require nothing, and the first step of a set must.
# =============================================================================

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
STEP_SETS = ["scripts/steps", "scripts/steps-v5"]

STEP_RE = re.compile(r"^#\s*step:\s*(\S+)\s*$", re.MULTILINE)
REQUIRES_RE = re.compile(r"^#\s*requires:\s*(.+?)\s*$", re.MULTILINE)


def steps_of(directory):
    """Every step in one set, in the order the driver runs them."""
    out = []
    for path in sorted(directory.glob("*.sh")):
        text = path.read_text(encoding="utf-8")
        name = STEP_RE.search(text)
        if name is None:
            continue
        requires = REQUIRES_RE.search(text)
        needs = []
        if requires:
            for token in re.split(r"[,\s]+", requires.group(1)):
                token = token.strip()
                # A prose requires line -- "nothing", "a reachable cluster" --
                # is a note and not a reference. Only something shaped like a
                # step name is checked.
                if re.fullmatch(r"[A-Z]-\d{2}-[a-z0-9-]+", token):
                    needs.append(token)
        out.append((path, name.group(1), needs))
    return out


def main():
    findings = []
    checked = 0

    for rel in STEP_SETS:
        directory = ROOT / rel
        if not directory.is_dir():
            continue
        steps = steps_of(directory)
        position = {name: i for i, (_, name, _) in enumerate(steps)}
        for i, (path, name, needs) in enumerate(steps):
            checked += 1
            for need in needs:
                where = position.get(need)
                if where is None:
                    findings.append(
                        (path.relative_to(ROOT), name, need, "is not a step in this set")
                    )
                elif where > i:
                    findings.append(
                        (
                            path.relative_to(ROOT),
                            name,
                            need,
                            f"runs later, at position {where + 1} of {len(steps)}",
                        )
                    )
                elif where == i:
                    findings.append(
                        (path.relative_to(ROOT), name, need, "is the step itself")
                    )

    for path, name, need, why in findings:
        print(f"{path}: {name} requires {need}, which {why}")

    if findings:
        print()
        print(
            f"{len(findings)} step(s) require something that does not run first. "
            "A requires line a reader cannot trust is worse than none."
        )
        return 1

    print(f"Every requires runs earlier. {checked} step(s) checked.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
