#!/usr/bin/env bash
# Normalize one-line Go copyright headers to hack/boilerplate.go.txt (Apache-2.0 block).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOILERPLATE="${ROOT}/hack/boilerplate.go.txt"
SHORT='// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.'
updated=0
while IFS= read -r -d '' path; do
  if head -n1 "$path" | grep -qxF "$SHORT"; then
    tail -n +2 "$path" | cat "$BOILERPLATE" - >"${path}.tmp"
    mv "${path}.tmp" "$path"
    updated=$((updated + 1))
  fi
done < <(find "$ROOT" -name '*.go' -not -path '*/vendor/*' -print0)
echo "normalized ${updated} Go file header(s)"
