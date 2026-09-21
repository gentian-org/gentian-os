#!/usr/bin/env bash
# Validate every authorization model version under authz/model/: the DSL parses,
# its tests pass, and the committed model.json is what the DSL transforms to.
#
# v0 is what the running operator embeds; v1 is the target the director checks
# against (docs/plans/authorization-model.md). Both are verified until v0 is
# deleted with the code that embeds it.
#
# Tooling, first match wins: an `fga` on PATH; `go install` of the pinned CLI;
# the pinned CLI image under docker. The last exists so that a machine with no
# Go toolchain can still run `make test-policy-authz`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FGA_VERSION="v0.7.20"

if command -v fga >/dev/null 2>&1; then
  fga_run() { local dir="$1"; shift; (cd "${dir}" && fga "$@"); }
elif command -v go >/dev/null 2>&1; then
  echo "Installing openfga/cli ${FGA_VERSION} via go install..."
  go install "github.com/openfga/cli/cmd/fga@${FGA_VERSION}"
  export PATH="${PATH}:$(go env GOPATH)/bin"
  fga_run() { local dir="$1"; shift; (cd "${dir}" && fga "$@"); }
elif command -v docker >/dev/null 2>&1; then
  echo "No fga and no go on PATH — using openfga/cli:${FGA_VERSION} under docker."
  fga_run() { local dir="$1"; shift; docker run --rm -v "${dir}:/w" -w /w "openfga/cli:${FGA_VERSION}" "$@"; }
else
  echo "FAIL — need one of: fga, go, docker." >&2
  exit 1
fi

status=0
for dir in "${ROOT}"/authz/model/v*/; do
  dir="${dir%/}"
  version="$(basename "${dir}")"
  echo "== authz model ${version}"

  fga_run "${dir}" model validate --file model.fga >/dev/null
  fga_run "${dir}" model test --tests tests.fga.yaml

  generated="$(mktemp)"
  fga_run "${dir}" model transform --file model.fga > "${generated}"
  if ! diff -q <(jq -S . "${generated}") <(jq -S . "${dir}/model.json") >/dev/null 2>&1; then
    echo "FAIL — ${version}/model.json is stale. Regenerate it:"
    echo "  fga model transform --file ${dir}/model.fga > ${dir}/model.json"
    diff -u <(jq -S . "${dir}/model.json") <(jq -S . "${generated}") | head -20
    status=1
  fi
  rm -f "${generated}"
done

[[ "${status}" -eq 0 ]] && echo "OK — every authz model version validates, passes its tests and matches its model.json."
exit "${status}"
