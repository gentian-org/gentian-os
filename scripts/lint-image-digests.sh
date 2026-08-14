#!/usr/bin/env bash
# =============================================================================
# scripts/lint-image-digests.sh — every pinned digest must be a manifest LIST
# =============================================================================
# A digest pin is good: it is the supply-chain guarantee a tag cannot give. But
# a digest can point at either a manifest list (every architecture) or a single
# manifest (exactly one). Pinning the second silently makes the image
# unpullable on any other architecture, and the failure surfaces as ImagePullBackOff
# on an arm64 node with a message that says nothing about architecture.
#
# The fix is never to drop the digest — that trades a real guarantee for a
# convenience. It is to pin the LIST digest, which keeps both.
#
# This cannot be checked statically: the digest string looks identical either
# way. So this lint asks the registry. That makes it network-dependent, which is
# why it is its own target rather than part of lint-shell, and why it skips
# rather than fails when a registry is unreachable — a lint that fails on a
# flaky network teaches people to ignore it.
#
# Usage:
#   scripts/lint-image-digests.sh
# =============================================================================

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; DIM=$'\033[2m'; NC=$'\033[0m'

fail=0; checked=0; skipped=0

# _is_manifest_list <repo> <digest> — 0 list, 1 single, 2 unknown.
_is_manifest_list() {
    local repo="$1" digest="$2" token body
    token="$(curl -fsS --max-time 20 \
        "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repo}:pull" \
        2>/dev/null | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')" || return 2
    [[ -n "${token}" ]] || return 2

    body="$(curl -fsS --max-time 20 -H "Authorization: Bearer ${token}" \
        -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
        "https://registry-1.docker.io/v2/${repo}/manifests/${digest}" 2>/dev/null)" || return 2

    case "${body}" in
        *'"manifests"'*) return 0 ;;
        *'"errors"'*)    return 2 ;;
        *)               return 1 ;;
    esac
}

echo ""
echo "Image digest lint — pinned digests must cover every architecture"
echo ""

# Only Docker Hub is queried; other registries need their own auth flow and are
# reported as unchecked rather than silently passed.
while IFS= read -r line; do
    file="${line%%:*}"
    rest="${line#*:}"
    digest="sha256:${rest##*@sha256:}"
    digest="${digest%%\"*}"

    repo="$(sed -n 's/.*repository:[[:space:]]*"\{0,1\}\([a-z0-9._/-]*\).*/\1/p' "${file}" | head -1)"
    [[ -n "${repo}" ]] || repo="$(basename "$(dirname "${file}")")"
    [[ "${repo}" == */* ]] || repo="library/${repo}"

    checked=$((checked + 1))
    if _is_manifest_list "${repo}" "${digest}"; then
        printf '  %s✓%s %-42s %s%s%s\n' "${GREEN}" "${NC}" "${repo}" "${DIM}" "${digest:0:19}… manifest list" "${NC}"
    else
        case $? in
            1)
                printf '  %s✗%s %-42s %s\n' "${RED}" "${NC}" "${repo}" "${digest:0:19}… SINGLE-ARCH"
                printf '      %s\n' "${file}"
                printf '      Pin the manifest-list digest instead. Do not drop the digest.\n'
                fail=1
                ;;
            *)
                printf '  %s○%s %-42s %s%s%s\n' "${YELLOW}" "${NC}" "${repo}" "${DIM}" "unreachable — not checked" "${NC}"
                skipped=$((skipped + 1))
                ;;
        esac
    fi
done < <(git ls-files 'charts/**/values.yaml' 'kernel/**/*.yaml' 2>/dev/null |
         xargs grep -l '@sha256:' 2>/dev/null |
         xargs grep -Hn "@sha256:" 2>/dev/null || true)

echo ""
if [[ ${checked} -eq 0 ]]; then
    echo "${GREEN}No digest pins found.${NC}"
    exit 0
fi
printf 'checked: %d   unreachable: %d\n' "${checked}" "${skipped}"
[[ ${fail} -eq 0 ]] || { echo "${RED}Single-architecture digest pin(s) found.${NC}"; exit 1; }
echo "${GREEN}Every pinned digest is a manifest list.${NC}"
