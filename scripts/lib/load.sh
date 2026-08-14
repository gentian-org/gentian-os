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

# -E propagates the ERR trap into functions, command substitutions and
# subshells. Without it gentian_report_abort (registered at the bottom of this
# file) would only fire for top-level commands, which is exactly where install
# failures do NOT happen — they happen deep inside step functions.
set -Eeuo pipefail

__GENTIAN_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__GENTIAN_SCRIPTS_DIR="$(cd "${__GENTIAN_LIB_DIR}/.." && pwd)"
# Repo root when callers (install.sh, update.sh, uninstall.sh) set SCRIPT_DIR first.
SCRIPT_DIR="${SCRIPT_DIR:-$(cd "${__GENTIAN_SCRIPTS_DIR}/.." && pwd)}"

# Component pins first: common.sh reads them at load time, and versions.sh has
# no dependencies of its own by design.
# shellcheck source=scripts/lib/versions.sh
source "${__GENTIAN_LIB_DIR}/versions.sh"
# shellcheck source=scripts/lib/compat.sh
source "${__GENTIAN_LIB_DIR}/compat.sh"

# shellcheck source=scripts/lib/lib-runtime.sh
source "${__GENTIAN_LIB_DIR}/lib-runtime.sh"

for _gentian_lib in portforward common certs openbao argocd catalogue validators credentials recovery bootstrap teardown; do
    # shellcheck source=/dev/null
    source "${__GENTIAN_LIB_DIR}/${_gentian_lib}.sh"
done
unset _gentian_lib

# Mail delivery helpers (MAIL_SERVICE_MODE, Postfix ConfigMap patching).
# shellcheck source=scripts/lib/mail-lib.sh
source "${__GENTIAN_LIB_DIR}/mail-lib.sh"

# LiteLLM Team reconciliation (one Team per Tenant CR).
# shellcheck source=scripts/lib/llm-lib.sh
source "${__GENTIAN_LIB_DIR}/llm-lib.sh"

# Post-install smoke checks (Keycloak OIDC, Dovecot TCP when kernel mail).
# shellcheck source=scripts/lib/verify-kernel-services.sh
source "${__GENTIAN_LIB_DIR}/verify-kernel-services.sh"

# Report where an aborted run died. Registered last so every library function is
# already defined. See gentian_report_abort in common.sh for the rationale.
trap gentian_report_abort ERR
