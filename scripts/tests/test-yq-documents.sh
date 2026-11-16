#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-yq-documents.sh
# =============================================================================
# The installer reads names out of files of several documents: the functions
# B-05 waits for, the definitions and compositions B-06 applies. Two programs
# are installed as `yq`. mikefarah/yq prints a `---` between the results of
# different documents unless told not to, kislyuk/yq prints none; a `---`
# read as a name stops the install at "functions.pkg.crossplane.io "---" not
# found".
#
#   - _gentian_yq and yq_get answer the names and nothing else, with the yq
#     that is installed here;
#   - the functions B-05 waits for are names, every one.
#
# No cluster, no network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"
# shellcheck source=scripts/lib/versions.sh
source scripts/lib/versions.sh
# yq_get alone, without the rest of the installer's library.
eval "$(sed -n '/^yq_get() {$/,/^}$/p' scripts/lib/common.sh)"

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
cat > "${SANDBOX}/several.yaml" <<'YAML'
kind: Function
metadata: {name: first}
---
kind: Provider
metadata: {name: other}
---
kind: Function
metadata: {name: second}
YAML

echo "yq: $(yq --version 2>&1 | head -1)"
filter='select(.kind == "Function") | .metadata.name'
want="first second"

# names <what> <the helper>: it answers the two names and nothing else.
names() {
    local got
    got="$("$2" "${filter}" "${SANDBOX}/several.yaml" | tr '\n' ' ')"
    if [[ "${got% }" == "${want}" ]]; then ok "$1"; else bad "$1" "    got: ${got}"; fi
}
names "_gentian_yq answers the names of several documents and no separator" _gentian_yq
names "yq_get answers the names of several documents and no separator" yq_get

declared="$(_gentian_yq "${filter}" crossplane/providers/providers.yaml)"
if [[ -n "${declared}" ]]; then ok "the providers file declares functions"; else bad "the providers file declares functions"; fi
odd="$(grep -vE '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' <<<"${declared}" || true)"
if [[ -z "${odd}" ]]; then ok "every function B-05 waits for is a name"; else bad "every function B-05 waits for is a name" "    not a name: ${odd}"; fi

finish
