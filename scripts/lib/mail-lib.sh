#!/usr/bin/env bash
# =============================================================================
# scripts/lib/mail-lib.sh — MAIL_SERVICE_MODE install/update helpers
# =============================================================================
# Sourced from scripts/lib/load.sh (and therefore install.sh).
#
# MAIL_SERVICE_MODE:
#   external — apps (Keycloak, etc.) send mail via EXTERNAL_SMTP_HOST directly
#   system   — deploy in-cluster Postfix + Dovecot; Keycloak uses Postfix
# =============================================================================

# Guard against double-sourcing.
[[ -n "${GENTIAN_MAIL_LIB_LOADED:-}" ]] && return 0
GENTIAN_MAIL_LIB_LOADED=1

mail_network_mode_compatible() {
    local mode="${1:-$(gentian_mail_service_mode)}"
    local network="${2:-${NETWORK_MODE:-tunnel}}"
    [[ "${mode}" != "system" || "${network}" != "tunnel" ]]
}

mail_network_mode_incompatibility_message() {
    cat <<'EOF'
MAIL_SERVICE_MODE=system is incompatible with NETWORK_MODE=tunnel.
Cloudflare tunnel exposes HTTP/HTTPS only — it cannot receive inbound SMTP (ports 25/587) or act as a public MX endpoint.
Use MAIL_SERVICE_MODE=external with EXTERNAL_SMTP_HOST + SMTP_RELAY_* credentials for invitation and outbound mail on tunnel clusters, or switch to NETWORK_MODE=static-ip when you have a reachable SMTP ingress.
EOF
}
