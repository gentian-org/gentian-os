#!/usr/bin/env bash
# Run OpenFGA model tests for authz/model/v0 (requires `fga` CLI).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODEL_DIR="${ROOT}/authz/model/v0"

if ! command -v fga >/dev/null 2>&1; then
  echo "Installing openfga/cli via go install..."
  go install github.com/openfga/cli/cmd/fga@latest
  gopath_bin="$(go env GOPATH)/bin"
  export PATH="${PATH}:${gopath_bin}"
fi

echo "Validating authorization model..."
fga model validate --file "${MODEL_DIR}/model.fga"

echo "Running model tests..."
fga model test --tests "${MODEL_DIR}/tests.fga.yaml"

echo "OK — Gentian authz model v0 tests passed."
