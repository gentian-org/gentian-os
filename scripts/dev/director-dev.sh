#!/usr/bin/env bash
# Run director-dev: the real director API with local stand-ins for Keycloak,
# OpenFGA, gentian-deployments and the App Store, for building a UI against it
# without a cluster. See cmd/director-dev/main.go.
#
#   scripts/dev/director-dev.sh [-entitlements] [-cors http://localhost:5173]
#
# Serves on 127.0.0.1:8090. Uses a local `go` when there is one, the golang
# image under docker otherwise.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PORT="${DIRECTOR_DEV_PORT:-8090}"

if command -v go >/dev/null 2>&1; then
  cd "${ROOT}" && exec go run ./cmd/director-dev -listen "127.0.0.1:${PORT}" "$@"
fi
command -v docker >/dev/null 2>&1 || { echo "FAIL — need go or docker." >&2; exit 1; }
exec docker run --rm -it -p "127.0.0.1:${PORT}:${PORT}" \
  -v "${ROOT}:/src:ro" -w /src \
  -v gentian-gomod:/go/pkg/mod -v gentian-gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  golang:1.25 go run ./cmd/director-dev -listen "0.0.0.0:${PORT}" -url "http://127.0.0.1:${PORT}" "$@"
