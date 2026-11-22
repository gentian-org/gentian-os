#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-listener-tag.sh
# =============================================================================
# The identity appset names the Keycloak event listener image by the hash of
# its sources (scripts/tools/listener-tag.sh). A change to the listener that
# left the line as it was would deploy the old jar; this fails it and prints
# the line to write.
# =============================================================================
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

appset="kernel/appsets/raw/09-identity.yaml"
want="$(bash scripts/tools/listener-tag.sh)"
image="ghcr.io/gentian-org/keycloak-event-listener"
named="$(grep -oE "${image}:[A-Za-z0-9._-]+" "${appset}" | sort -u)"

if [[ "${named}" != "${image}:${want}" ]]; then
    echo "lint-listener-tag: ${appset} names ${named:-no listener image}" >&2
    echo "  The listener's sources hash to ${want}. Write:" >&2
    echo "    value: '${image}:${want}'" >&2
    exit 1
fi
echo "OK — lint-listener-tag: ${appset} names ${image}:${want}"
