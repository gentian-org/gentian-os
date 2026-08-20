#!/usr/bin/env python3
# =============================================================================
# scripts/gen/gen-phase-table.py — the phase table, derived from the phases
# =============================================================================
# §11's summary table and the phase sections were two records of one set of
# facts, and a change updated one of them. In a single week the table said
# Phase 6 was unverified while §15.1 recorded both its criteria verified; said
# the CA-bundle contract was undefined in the same commit that defined it; and
# said a refused token exchange produces no log line months after it started
# producing one. Each was found by reading, not by anything failing.
#
# That is the defect this plan catalogues everywhere else — one fact in two
# places with nothing enforcing agreement — so the table is now generated from
# the sections and checked in CI.
#
# The contract is one line per phase section:
#
#   ### Phase 7 — OIDC write path and root token revocation
#
#   **State — Exercised.** The live OIDC write path works. …
#
# `**State — <state>.**` is the state; the rest of the line is what is left.
# Both are prose meant for a reader — nothing here invents a vocabulary of
# statuses, because the useful thing about "4a exercised, 4b half" is exactly
# what a fixed enum would throw away.
#
# Usage:
#   scripts/gen/gen-phase-table.py            rewrite the table in place
#   scripts/gen/gen-phase-table.py --check    fail if it is out of date
# =============================================================================

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DOC = ROOT / "docs" / "plans" / "config-and-credential-cleanup.md"

BEGIN = "<!-- BEGIN generated phase table -->"
END = "<!-- END generated phase table -->"

HEADING = re.compile(r"^### Phase ([0-9]+[ab]?) — (.+)$")
STATE = re.compile(r"^\*\*State — (.+?)\.\*\*\s*(.*)$")


def phases(text: str) -> list[tuple[str, str, str]]:
    """(phase, state, what is left), in document order."""
    out: list[tuple[str, str, str]] = []
    lines = text.splitlines()
    for i, line in enumerate(lines):
        m = HEADING.match(line)
        if not m:
            continue
        phase = m.group(1)
        # The state line is the first non-blank line of the section. Requiring
        # it to be first is what keeps it visible to a reader rather than
        # drifting into the middle of the prose.
        for probe in lines[i + 1 : i + 6]:
            if not probe.strip():
                continue
            sm = STATE.match(probe)
            if sm:
                out.append((phase, sm.group(1).strip(), sm.group(2).strip()))
            else:
                sys.exit(
                    f"FATAL: Phase {phase} does not open with a state line.\n"
                    f"       Expected: **State — <state>.** <what is left>\n"
                    f"       Found:    {probe[:70]}"
                )
            break
    return out


def render(rows: list[tuple[str, str, str]]) -> str:
    body = ["| Phase | State | What is left |", "|---|---|---|"]
    for phase, state, left in rows:
        body.append(f"| {phase} | {state} | {left} |")
    return "\n".join(body)


def main() -> int:
    check = "--check" in sys.argv
    text = DOC.read_text()
    if BEGIN not in text or END not in text:
        sys.exit(f"FATAL: {DOC.name} has no {BEGIN} / {END} markers.")

    rows = phases(text)
    if not rows:
        sys.exit("FATAL: no phase sections parsed — has the heading form changed?")

    head, rest = text.split(BEGIN, 1)
    _, tail = rest.split(END, 1)
    updated = f"{head}{BEGIN}\n{render(rows)}\n{END}{tail}"

    if updated == text:
        print(f"\033[0;32mPhase table matches the phases\033[0m ({len(rows)} phases).")
        return 0
    if check:
        print(
            "\033[0;31mERROR\033[0m The phase table disagrees with the phase sections.\n"
            "      The sections are the record. Run: make gen-phase-table",
            file=sys.stderr,
        )
        return 1
    DOC.write_text(updated)
    print(f"rewrote the phase table from {len(rows)} phase sections")
    return 0


if __name__ == "__main__":
    sys.exit(main())
