#!/usr/bin/env bash
# Convert a catalogue of AppProfiles and prove the result is admissible: every
# converted ComponentProfile is run through the generated CRD's OpenAPI schema
# and CEL rules, the way the API server would.
#
#   convert-appprofiles.sh <profiles-dir> <out-dir>
#
# Uses a local `go` when there is one, the golang image under docker otherwise.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SRC="${1:?usage: convert-appprofiles.sh <profiles-dir> <out-dir>}"
OUT="$(mkdir -p "${2:?usage: convert-appprofiles.sh <profiles-dir> <out-dir>}" && cd "$2" && pwd)"

python3 "${ROOT}/scripts/tools/convert-appprofile.py" "${SRC}" --out "${OUT}"

echo "== admission: converted profiles against the ComponentProfile CRD"
if command -v go >/dev/null 2>&1; then
  (cd "${ROOT}" && COMPONENT_PROFILE_DIR="${OUT}/profiles" go test -count=1 -run TestConvertedProfilesAreAdmitted -v ./api/v1alpha1/ | grep -E '^(---|===|\s+component_schema_test|ok|FAIL|PASS)' )
elif command -v docker >/dev/null 2>&1; then
  docker run --rm -v "${ROOT}:/src" -w /src -v "${OUT}/profiles:/profiles:ro" \
    -v "${GOMODCACHE_DIR:-gentian-gomod}:/go/pkg/mod" -v "${GOCACHE_DIR:-gentian-gocache}:/root/.cache/go-build" \
    -e GOFLAGS=-buildvcs=false -e COMPONENT_PROFILE_DIR=/profiles golang:1.25 \
    go test -count=1 -run TestConvertedProfilesAreAdmitted -v ./api/v1alpha1/ | grep -E '^(---|===|\s+component_schema_test|ok|FAIL|PASS)'
else
  echo "FAIL — need go or docker to check admission." >&2; exit 1
fi
echo "Review items: ${OUT}/REVIEW.md"
