#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-marker-ascii.py — a curly quote in a CEL rule breaks the CRD
# =============================================================================
# kubebuilder markers are copied into the CRD almost verbatim, and a CEL
# validation rule is then compiled by the API server. CEL's string literals are
# '' and "". A typographic quote — ” or ’, the kind an editor or a paste
# substitutes without asking — is not a token CEL knows:
#
#   CustomResourceDefinition "tenantexports.gentianos.io" is invalid:
#   compilation failed: ERROR: <input>:1:66: Syntax error: token recognition
#   error at: '”'
#
# The API server then refuses the whole CRD. Every test in the package that
# installs it panics before the first assertion, and on a cluster the type
# simply stops being installable.
#
# This has happened twice on the same file, a day apart, and neither time was it
# visible: `!= ''` and `!= ”` are one pixel apart in most fonts, the Go code
# compiles either way, gofmt is happy either way, and vet has no opinion. What
# caught it both times was an envtest panic several steps later — and the second
# time it reached the branch, because the CRD in git was still stale and only
# broke once someone regenerated it.
#
# WHAT IT CHECKS
#
# Every +kubebuilder: marker line is ASCII. Markers are machine input; there is
# no reason for a marker to carry an em dash or a curly quote, so the rule can
# be absolute rather than a judgement about which characters are dangerous.
#
# Prose comments are untouched. This repository writes — and … in comments
# deliberately and they never reach a CRD.
#
# Usage:
#   python3 scripts/lint/lint-marker-ascii.py
# =============================================================================
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SCAN = [ROOT / "api", ROOT / "internal", ROOT / "cmd"]
MARKER = "+kubebuilder:"

RED = "\033[0;31m"
GREEN = "\033[0;32m"
DIM = "\033[2m"
NC = "\033[0m"

# What a marker most often gets by accident, and what it meant to say. Named so
# the report can suggest the repair rather than only the offence.
LIKELY = {
    "“": "\" or ''",
    "”": "\" or ''",
    "‘": "'",
    "’": "'",
    "–": "-",
    "—": "-",
    " ": "a plain space",
}


def main() -> int:
    files = sorted(p for root in SCAN if root.is_dir() for p in root.rglob("*.go"))
    if not files:
        sys.exit("no Go sources found to scan, which is not the same as passing")

    bad, markers = [], 0
    for path in files:
        for number, line in enumerate(path.read_text(errors="replace").splitlines(), 1):
            if MARKER not in line:
                continue
            markers += 1
            for column, char in enumerate(line, 1):
                if ord(char) < 128:
                    continue
                bad.append((path.relative_to(ROOT), number, column, char, line.strip()))

    print("")
    print("kubebuilder markers — ASCII only, because the API server parses them")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    for path, number, column, char, line in bad:
        name = f"U+{ord(char):04X}"
        suggestion = LIKELY.get(char)
        print(f"  {RED}ERROR{NC} {path}:{number}:{column}: {name} {char!r}")
        if suggestion:
            print(f"        {DIM}probably meant {suggestion}{NC}")
        print(f"        {DIM}{line[:100]}{NC}")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    if bad:
        print(f"{RED}{len(bad)} non-ASCII character(s) in kubebuilder markers.{NC}")
        print(f"{DIM}A marker is machine input. In a CEL rule this is not a typo{NC}")
        print(f"{DIM}the compiler forgives — the API server refuses the CRD, and{NC}")
        print(f"{DIM}every test that installs it panics before its first assertion.{NC}")
        return 1
    print(f"{GREEN}Every kubebuilder marker is ASCII{NC} ({markers} marker(s) in "
          f"{len(files)} file(s)).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
