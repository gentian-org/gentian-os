#!/usr/bin/env bash
# Run OpenFGA model tests for authz/model/v0 (requires `fga` CLI).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODEL_DIR="${ROOT}/authz/model/v0"

# Pinned, not @latest. @latest broke CI the day openfga/cli v0.7.20 raised its
# Go floor to 1.25.7 — go.mod was 1.25.0 at the time and is 1.25.7 now
# (go 1.25.7 happens to be exactly what v0.7.20 requires, so no GOTOOLCHAIN
# override is needed to build it).
#
# Pinned here rather than in versions.yaml, which is the mirror inventory for an
# air-gapped install: driver.sh's validate_pins requires every entry there to be
# claimed by a step with `# pins:`, and no step pulls this. It is a test tool,
# so it belongs with its only caller.
FGA_VERSION="v0.7.20"

if ! command -v fga >/dev/null 2>&1; then
  echo "Installing openfga/cli ${FGA_VERSION} via go install..."
  go install "github.com/openfga/cli/cmd/fga@${FGA_VERSION}"
  gopath_bin="$(go env GOPATH)/bin"
  export PATH="${PATH}:${gopath_bin}"
fi

echo "Validating authorization model..."
fga model validate --file "${MODEL_DIR}/model.fga"

echo "Running model tests..."
fga model test --tests "${MODEL_DIR}/tests.fga.yaml"

# model.json is the compiled form of model.fga. Nothing reads it at runtime
# today, so a divergence would surface only when something finally did — as an
# authorization model that does not match the one the tests just passed
# against. Regenerating and comparing is cheaper than remembering.
echo "Checking model.json matches model.fga..."
generated="$(mktemp)"
trap 'rm -f "${generated}"' EXIT
fga model transform --file "${MODEL_DIR}/model.fga" > "${generated}"
if ! diff -q <(jq -S . "${generated}") <(jq -S . "${MODEL_DIR}/model.json") >/dev/null 2>&1; then
  echo "FAIL — model.json is stale. Regenerate it:"
  echo "  fga model transform --file ${MODEL_DIR}/model.fga > ${MODEL_DIR}/model.json"
  diff -u <(jq -S . "${MODEL_DIR}/model.json") <(jq -S . "${generated}") | head -20
  exit 1
fi

echo "OK — Gentian authz model v0 tests passed."
