#!/usr/bin/env bash
# =============================================================================
# scripts/tests/lib/harness.sh — how a test under scripts/tests reports
# =============================================================================
# Sourced by the tests, never run:
#
#   # shellcheck source=scripts/tests/lib/harness.sh
#   source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"
#
#   ok <what>                      one check passed
#   bad <what> [detail]            one check failed, and what it saw
#   check <what> <test> [detail]   ok or bad, as the test handed to eval says
#   finish                         the count, and the test's exit status
#
# A test with a check of another shape defines its own check after sourcing
# this; ok, bad and finish are the same for every test.
# =============================================================================

# shellcheck disable=SC2034 # YELLOW is for the tests that report a skip.
GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
pass=0; fail=0

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1" "${3:-}"; fi; }

# finish — print how many checks passed and failed, and exit 1 when any failed.
finish() {
    echo ""
    if (( fail > 0 )); then
        printf '%s%d failed%s, %d passed.\n' "${RED}" "${fail}" "${NC}" "${pass}"
        exit 1
    fi
    printf '%s%d checks passed.%s\n' "${GREEN}" "${pass}" "${NC}"
    exit 0
}
