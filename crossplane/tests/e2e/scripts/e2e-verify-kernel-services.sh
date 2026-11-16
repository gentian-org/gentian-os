#!/usr/bin/env bash
# E2E smoke — kernel mail (Dovecot) health (live cluster required).
#
# Usage:  make verify-kernel-services
#         VERIFY_KERNEL_SERVICES=1 crossplane/tests/e2e/scripts/e2e-verify-kernel-services.sh
#
# Prerequisites:
#   - Gentian OS install complete
#   - MAIL_SERVICE_MODE=system and dovecot-dev deployed; otherwise the check
#     is skipped
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"

# shellcheck source=scripts/lib/load.sh
source "${REPO_ROOT}/scripts/lib/load.sh"

load_deployments_cluster_settings 2>/dev/null || true

info()  { echo "[INFO]  $*"; }
pass()  { echo "[PASS]  $*"; }
fail()  { echo "[FAIL]  $*"; exit 1; }

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found"
kubectl cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || fail "No reachable cluster — point KUBECONFIG at your dev cluster."

if verify_dovecot_installation; then
    pass "Dovecot installation verification (or skipped when MAIL_SERVICE_MODE≠system)"
else
    fail "Dovecot installation verification failed"
fi
