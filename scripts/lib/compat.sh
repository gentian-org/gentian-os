#!/usr/bin/env bash
# =============================================================================
# scripts/lib/compat.sh — BSD/GNU divergences, in one place
# =============================================================================
# macOS is a supported install host. That means two constraints, and they are
# different problems:
#
#   1. Stock /bin/bash on macOS is 3.2, from 2007. No `declare -A`, no
#      `mapfile`, no `${var^^}`. Requiring a Homebrew bash would work, but it
#      adds a prerequisite to a list §1 is trying to shorten — and the
#      constructs have cheap 3.2-compatible equivalents.
#
#   2. macOS ships BSD userland. `sed -i` takes a mandatory argument there and
#      forbids one under GNU, so the same line cannot work on both. That one
#      genuinely needs a wrapper.
#
# This file holds the wrappers. The lint in `make lint-portability` holds the
# line on the constructs, so a ninth bash-4 site cannot appear while the eight
# known ones are being migrated.
#
# Deliberately dependency-free, like versions.sh: standalone scripts need
# sed_inplace without loading the install library.
# =============================================================================

[[ -n "${GENTIAN_COMPAT_LOADED:-}" ]] && return 0
GENTIAN_COMPAT_LOADED=1

# sed_inplace <sed-expression> <file> [file...]
#
# In-place edit that behaves identically under BSD and GNU sed.
#
# The divergence: GNU `sed -i` takes no argument and treats a following string
# as the expression; BSD `sed -i` REQUIRES a backup suffix and treats the next
# argument as that suffix. `sed -i ''` works on BSD and fails on GNU; `sed -i`
# alone works on GNU and eats the expression on BSD.
#
# Writing to a temporary file and moving it over sidesteps the flag entirely,
# which is why this does not try to detect which sed is present. Detection would
# be one more thing to get wrong on a platform nobody tests on.
sed_inplace() {
    local expr="$1"; shift
    [[ $# -gt 0 ]] || { echo "sed_inplace: no files given" >&2; return 2; }

    local f tmp
    for f in "$@"; do
        [[ -f "$f" ]] || { echo "sed_inplace: no such file: $f" >&2; return 1; }
        tmp="$(mktemp "${TMPDIR:-/tmp}/gentian-sed.XXXXXX")" || return 1
        if ! sed "${expr}" "$f" > "${tmp}"; then
            rm -f "${tmp}"
            return 1
        fi
        # cat-and-truncate rather than mv, so the destination keeps its
        # ownership, permissions and any hard links. mv would replace the inode,
        # which matters for the one site that edits a root-owned kubelet arg file.
        cat "${tmp}" > "$f"
        rm -f "${tmp}"
    done
}

# to_upper / to_lower — ${var^^} and ${var,,} need bash 4.
to_upper() { printf '%s' "$1" | tr '[:lower:]' '[:upper:]'; }
to_lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# xargs_r <command...> — run only when stdin is non-empty.
#
# GNU xargs has -r for this; BSD xargs makes it the default and older BSD
# rejects the flag outright. Neither behaviour is safe to assume, and dropping
# -r changes GNU semantics (it would run the command once with no arguments),
# so the emptiness test moves here where it is explicit.
xargs_r() {
    local input
    input="$(cat)"
    [[ -n "${input}" ]] || return 0
    printf '%s\n' "${input}" | xargs "$@"
}
