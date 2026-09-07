#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-live-identifiers.sh — this repo describes a platform, not
# one deployment of it
# =============================================================================
# gentian-os is meant to be app-, tenant- and server-agnostic. 774027f purged
# the identifiers of one specific deployment from 55 files and said so in its
# message — and three days later `gtn.host` was back in api/v1alpha1's tests,
# and again three days after that. Nothing was watching, so the cleanup lasted
# exactly as long as nobody wrote a comment.
#
# That is the whole reason this exists. Every occurrence is individually
# harmless: an example in a doc comment, a fixture domain in a table test, a
# cluster named in a "seen in practice" note. None changes behaviour. What they
# do is make the repository read as if one cluster were the only cluster, and
# they accumulate precisely because each one looks too small to argue about.
#
# Reported, not fatal, for the same reason lint-claim-defaults is: there are
# some today, each needs a judgement about whether to generalise the example or
# delete the sentence, and a check that failed the build on arrival would just
# be switched off. The contract is the one that worked for lint-portability
# until it reached zero: THE NUMBER MUST ONLY GO DOWN.
#
# What is deliberately NOT listed: public endpoints of third-party services
# (api.cloudflare.com, sos-ch-dk-2.exo.io). Naming the provider's own address is
# documentation, not a statement about who runs this.
#
# Usage:
#   scripts/lint/lint-live-identifiers.sh [--strict]
# =============================================================================

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

STRICT=0
[[ "${1:-}" == "--strict" ]] && STRICT=1

RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; GREEN=$'\033[0;32m'; DIM=$'\033[2m'; NC=$'\033[0m'

# Parallel arrays, not an associative one: macOS ships bash 3.2, which has no
# `declare -A` (lint-portability).
LABELS=(
    "cluster id"
    "cluster id"
    "platform domain"
    "build host"
    "tenant name"
)
PATTERNS=(
    "ifk-w4h"
    "pck-kulxwmm"
    "gtn\.host"
    "beefy1"
    "tenant-corp|--tenant corp|@corp\.|tenant/corp"
)

# This file names every pattern it looks for, so scanning itself would report
# one hit per rule — the mistake lint-portability made until it excluded itself.
files() {
    git ls-files -- '*.go' '*.sh' '*.py' '*.yaml' '*.yml' '*.md' '*.tmpl' 2>/dev/null |
        grep -v '^scripts/lint/lint-live-identifiers\.sh$'
}

echo ""
echo "Live-identifier lint — does this repo name one deployment?"
echo ""

# `while read`, not mapfile: bash 3.2 has neither.
_files=()
while IFS= read -r _f; do
    [[ -n "${_f}" ]] && _files+=("${_f}")
done < <(files)

total=0
i=0
while (( i < ${#PATTERNS[@]} )); do
    label="${LABELS[$i]}"
    pattern="${PATTERNS[$i]}"
    hits="$(grep -nIE "${pattern}" "${_files[@]}" 2>/dev/null || true)"
    n=0
    [[ -n "${hits}" ]] && n="$(printf '%s\n' "${hits}" | wc -l | tr -d ' ')"
    total=$(( total + n ))

    if (( n == 0 )); then
        printf '  %s✓%s %-16s %-44s %s\n' "${GREEN}" "${NC}" "${label}" "${pattern}" "0"
    else
        printf '  %s•%s %-16s %-44s %s\n' "${YELLOW}" "${NC}" "${label}" "${pattern}" "${n}"
        printf '%s\n' "${hits}" | cut -d: -f1 | sort -u |
            sed "s/^/        ${DIM}/;s/\$/${NC}/"
    fi
    i=$(( i + 1 ))
done

echo ""
if (( total == 0 )); then
    printf '%sNo deployment-specific identifiers.%s\n' "${GREEN}" "${NC}"
    exit 0
fi

printf '%s%d occurrence(s) naming one specific deployment.%s\n' "${YELLOW}" "${total}" "${NC}"
echo "Each is an example that could name nothing in particular instead."
echo "The number must only go down — 75 when this check was written."
(( STRICT )) && { printf '%s--strict: failing.%s\n' "${RED}" "${NC}"; exit 1; }
exit 0
