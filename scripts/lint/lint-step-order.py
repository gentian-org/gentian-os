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
STEP_SETS = ["scripts/steps"]

STEP_RE = re.compile(r"^#\s*step:\s*(\S+)\s*$", re.MULTILINE)
REQUIRES_RE = re.compile(r"^#\s*requires:\s*(.+?)\s*$", re.MULTILINE)
PHASE_RE = re.compile(r"^#\s*phase:\s*(\S+)\s*$", re.MULTILINE)

# The phase a step's letter stands for. The installer prints a banner when the
# phase changes and --phase selects by it, so a step that declares another
# phase than its letter's splits a phase in two on screen and is skipped by
# --phase. C-04 once declared "claims" (no such phase) and D-01 "platform".
PHASES = {"A": "control-plane", "B": "secrets", "C": "platform", "D": "applications", "E": "handover"}


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
        phase = PHASE_RE.search(text)
        out.append((path, name.group(1), needs, phase.group(1) if phase else None))
    return out


def main():
    findings = []
    phase_findings = []
    checked = 0

    for rel in STEP_SETS:
        directory = ROOT / rel
        if not directory.is_dir():
            continue
        steps = steps_of(directory)
        position = {name: i for i, (_, name, _, _) in enumerate(steps)}
        for i, (path, name, needs, phase) in enumerate(steps):
            checked += 1
            want = PHASES.get(name[:1])
            if want is not None and phase != want:
                phase_findings.append((path.relative_to(ROOT), name, phase, want))
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
    for path, name, phase, want in phase_findings:
        print(f"{path}: {name} declares phase {phase!r}; a {name[:1]} step is {want!r}")

    if phase_findings:
        print()
        print(
            f"{len(phase_findings)} step(s) in another phase than their letter's: "
            "the banner splits the phase on screen and --phase skips them."
        )
        if not findings:
            return 1

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
