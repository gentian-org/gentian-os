#!/usr/bin/env bash
# =============================================================================
# scripts/tools/listener-tag.sh [REPO_ROOT]
# =============================================================================
# Prints the Keycloak event listener image's tag: src-<12 hex>, a hash of the
# listener's tracked files (path and content). The image is named by what it
# is built from and not by the platform's revision, so a build that leaves the
# listener alone leaves Keycloak's pod alone too -- no restart, and no wait on
# an image a failed build never published.
#
# The identity appset names this tag (kernel/appsets/raw/09-identity.yaml,
# held by lint-listener-tag), CI builds it once (.github/workflows/ci.yaml),
# and the installer's pre-flight asks for it (check_build_images).
# =============================================================================
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
dir="kernel/extensions/keycloak-event-listener"
cd "${root}"

if command -v sha256sum >/dev/null 2>&1; then
    sum() { sha256sum "$@"; }
else
    sum() { shasum -a 256 "$@"; }
fi

files=()
while IFS= read -r -d '' f; do
    files+=("${f}")
done < <(git ls-files -z -- "${dir}" | LC_ALL=C sort -z)
[[ ${#files[@]} -gt 0 ]] || { echo "listener-tag: no tracked files under ${dir}" >&2; exit 1; }

printf 'src-%s\n' "$(sum "${files[@]}" | sum | cut -c1-12)"
