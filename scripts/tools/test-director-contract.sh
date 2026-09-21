#!/usr/bin/env bash
# Run the director's contract tests against a real OpenFGA holding model v1 and
# the fixture of authz/model/v1/tests.fga.yaml.
#
# `go test ./internal/director/...` alone answers authorization from a table
# that states what the model should say. This run is what shows the table and
# the model agree: the same tests, with OpenFGA deciding.
#
# Needs docker. Uses a local `go` when there is one, the golang image otherwise.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OPENFGA_IMAGE="openfga/openfga:v1.8.16"
GO_IMAGE="golang:1.25"
NAME="director-contract-openfga-$$"

command -v docker >/dev/null 2>&1 || { echo "FAIL — docker is required to run OpenFGA." >&2; exit 1; }

cleanup() { docker rm -f "${NAME}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run -d --rm --name "${NAME}" -p 127.0.0.1::8080 "${OPENFGA_IMAGE}" run >/dev/null
PORT="$(docker port "${NAME}" 8080/tcp | head -1 | sed 's/.*://')"
URL="http://127.0.0.1:${PORT}"

for _ in $(seq 1 60); do
  if curl -fsS "${URL}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
curl -fsS "${URL}/healthz" >/dev/null || { echo "FAIL — OpenFGA did not become healthy." >&2; docker logs "${NAME}" | tail -20 >&2; exit 1; }

echo "== director contract tests, decisions by ${OPENFGA_IMAGE}"
if command -v go >/dev/null 2>&1; then
  (cd "${ROOT}" && DIRECTOR_TEST_OPENFGA_URL="${URL}" go test -count=1 ./internal/director/...)
else
  docker run --rm --network host \
    -v "${ROOT}:/src" -w /src \
    -v "${GOMODCACHE_DIR:-director-contract-gomod}:/go/pkg/mod" \
    -v "${GOCACHE_DIR:-director-contract-gocache}:/root/.cache/go-build" \
    -e GOFLAGS=-buildvcs=false -e DIRECTOR_TEST_OPENFGA_URL="${URL}" \
    "${GO_IMAGE}" go test -count=1 ./internal/director/...
fi
