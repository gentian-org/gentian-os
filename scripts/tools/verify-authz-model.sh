#!/usr/bin/env bash
# Run OpenFGA model tests for authz/model/v0 (requires `fga` CLI).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODEL_DIR="${ROOT}/authz/model/v0"

if ! command -v fga >/dev/null 2>&1; then
  # Pinned, not @latest. @latest broke CI the day openfga/cli v0.7.20 raised its
  # Go floor to 1.25.7 while this project's go.mod pins 1.25.0 — a red build
  # caused by an upstream release rather than by any change here, which is the
  # whole reason versions.yaml exists.
  #
  # GOTOOLCHAIN=auto for this command only: every published fga needs a newer Go
  # than the project targets, and setup-go sets GOTOOLCHAIN=local, so Go refuses
  # to fetch one. Scoped to the install, the project's own toolchain is untouched.
  FGA_VERSION="$(bash "$(dirname "${BASH_SOURCE[0]}")/../lib/versions.sh" openfga cli)"
  echo "Installing openfga/cli ${FGA_VERSION} via go install..."
  GOTOOLCHAIN=auto go install "github.com/openfga/cli/cmd/fga@${FGA_VERSION}"
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
