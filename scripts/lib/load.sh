#!/usr/bin/env bash
# =============================================================================
# scripts/lib/load.sh — Gentian install library loader
# =============================================================================
# Source this file from install.sh, update.sh, and uninstall.sh to load all
# install helper functions without running any of their main flows.
#
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   # shellcheck source=scripts/lib/load.sh
#   source "${SCRIPT_DIR}/scripts/lib/load.sh"
# =============================================================================

if [[ -n "${GENTIAN_LIB_LOADED:-}" ]]; then
    # shellcheck disable=SC2317
    return 0 2>/dev/null || exit 0
fi
GENTIAN_LIB_LOADED=1

set -euo pipefail

__GENTIAN_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__GENTIAN_SCRIPTS_DIR="$(cd "${__GENTIAN_LIB_DIR}/.." && pwd)"
# Repo root when callers (install.sh, update.sh, uninstall.sh) set SCRIPT_DIR first.
SCRIPT_DIR="${SCRIPT_DIR:-$(cd "${__GENTIAN_SCRIPTS_DIR}/.." && pwd)}"

# shellcheck source=scripts/lib-runtime.sh
source "${__GENTIAN_SCRIPTS_DIR}/lib-runtime.sh"

for _gentian_lib in common certs openbao argocd catalogue; do
    # shellcheck source=/dev/null
    source "${__GENTIAN_LIB_DIR}/${_gentian_lib}.sh"
done
unset _gentian_lib

# Mail delivery helpers (MAIL_SERVICE_MODE, Postfix ConfigMap patching).
# shellcheck source=scripts/mail-lib.sh
source "${__GENTIAN_SCRIPTS_DIR}/mail-lib.sh"

# LiteLLM Team reconciliation (one Team per Tenant CR).
# shellcheck source=scripts/llm-lib.sh
source "${__GENTIAN_SCRIPTS_DIR}/llm-lib.sh"

# Post-install smoke checks (Keycloak OIDC, Dovecot TCP when kernel mail).
# shellcheck source=scripts/verify-kernel-services.sh
source "${__GENTIAN_SCRIPTS_DIR}/verify-kernel-services.sh"
