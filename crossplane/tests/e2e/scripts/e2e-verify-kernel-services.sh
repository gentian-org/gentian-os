#!/usr/bin/env bash
# E2E smoke — Keycloak + Dovecot installation health (live cluster required).
#
# Usage:  make e2e-p5-keycloak-dovecot
#         VERIFY_KERNEL_SERVICES=1 ./scripts/e2e-verify-kernel-services.sh
#
# Prerequisites:
#   - Gentian OS Stage 1 install complete (Suze / Keycloak in platform-kernel)
#   - For Dovecot checks: MAIL_SERVICE_MODE=kernel and dovecot-dev deployed
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"

export GENTIAN_INSTALL_LIB_ONLY=1
# shellcheck source=scripts/lib/load.sh
source "${REPO_ROOT}/scripts/lib/load.sh"
unset GENTIAN_INSTALL_LIB_ONLY

load_deployments_cluster_settings 2>/dev/null || true

info()  { echo "[INFO]  $*"; }
pass()  { echo "[PASS]  $*"; }
fail()  { echo "[FAIL]  $*"; exit 1; }

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found"
kubectl cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || fail "No reachable cluster — point KUBECONFIG at your dev cluster."

ERRORS=0

if verify_keycloak_installation; then
    pass "Keycloak installation verification"
else
    ERRORS=$((ERRORS + 1))
fi

if verify_dovecot_installation; then
    pass "Dovecot installation verification (or skipped when MAIL_SERVICE_MODE≠kernel)"
else
    ERRORS=$((ERRORS + 1))
fi

if [[ "${ERRORS}" -gt 0 ]]; then
    fail "${ERRORS} kernel service verification(s) failed"
fi

pass "All kernel service smoke checks passed"
