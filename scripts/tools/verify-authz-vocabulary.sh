#!/usr/bin/env bash
# One vocabulary: the relations the documents name, the relations the code
# checks, and the relations the model defines are the same set.
#
#   documents → model   a can_* in docs/plans or the security principles that the
#                       model does not define is a promise nothing keeps
#   model → documents   a can_* the model defines that authorization-model.md
#                       never mentions is a permission nobody decided on
#   code → model        a can_* the director names that the model does not define
#                       is a check that can only ever deny
#
# Relations planned but not yet modelled are listed, with the reason, in
# authz/model/<version>/planned.txt.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="${AUTHZ_MODEL_VERSION:-v1}"
MODEL="${ROOT}/authz/model/${VERSION}/model.fga"
PLANNED="${ROOT}/authz/model/${VERSION}/planned.txt"
VOCABULARY_DOC="${ROOT}/docs/plans/authorization-model.md"

[[ -f "${MODEL}" ]] || { echo "FAIL — no model at ${MODEL}" >&2; exit 1; }

model="$(grep -oE '^[[:space:]]+define[[:space:]]+[a-z_]+' "${MODEL}" | awk '{print $2}' | sort -u)"
planned="$(grep -vE '^[[:space:]]*(#|$)' "${PLANNED}" 2>/dev/null | awk '{print $1}' | sort -u || true)"

documents=()
while IFS= read -r f; do documents+=("$f"); done < <(find "${ROOT}/docs/plans" -maxdepth 1 -name '*.md' | sort)
documents+=("${ROOT}/docs/security-principles.md")

status=0
fail() { echo "FAIL — $*" >&2; status=1; }

for rel in $(grep -ohE '\bcan_[a-z_]+\b' "${documents[@]}" | sort -u); do
  grep -qx "${rel}" <<<"${model}" && continue
  grep -qx "${rel}" <<<"${planned}" && continue
  where="$(grep -lE "\b${rel}\b" "${documents[@]}" | sed "s|${ROOT}/||" | tr '\n' ' ')"
  fail "${rel} is named in ${where}but model ${VERSION} does not define it (add it, or list it in planned.txt with the reason)"
done

for rel in $(grep -E '^can_' <<<"${model}"); do
  grep -qE "\b${rel}\b" "${VOCABULARY_DOC}" || fail "${rel} is defined in model ${VERSION} but docs/plans/authorization-model.md never mentions it"
done

for rel in ${planned}; do
  grep -qx "${rel}" <<<"${model}" && fail "${rel} is in planned.txt and in the model: remove it from planned.txt"
done

while IFS= read -r hit; do
  file="${hit%%:*}"; rel="$(grep -oE 'can_[a-z_]+' <<<"${hit#*:}" | head -1)"
  grep -qx "${rel}" <<<"${model}" || fail "${file#"${ROOT}/"} checks ${rel}, which model ${VERSION} does not define"
done < <(grep -rnoE '"can_[a-z_]+"' "${ROOT}/internal/director" "${ROOT}/cmd/director" --include='*.go' --exclude='*_test.go' || true)

if [[ ${status} -eq 0 ]]; then
  echo "OK — authorization vocabulary: $(grep -cE '^can_' <<<"${model}") can_* relations in model ${VERSION}, documents and director agree."
fi
exit ${status}
