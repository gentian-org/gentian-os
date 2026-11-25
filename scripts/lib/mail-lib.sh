#!/usr/bin/env bash
# =============================================================================
# scripts/lib/mail-lib.sh — MAIL_SERVICE_MODE install/update helpers
# =============================================================================
# Sourced from scripts/lib/load.sh (and therefore install.sh).
#
# MAIL_SERVICE_MODE, the Cluster claim's mail.serviceMode:
#   relay    — apps and Keycloak send through a provider at EXTERNAL_SMTP_HOST;
#              no mailboxes
#   external — a mail server outside the cluster holds the mailboxes: apps and
#              Keycloak send through it at EXTERNAL_SMTP_HOST and read from it
#              at MAIL_IMAP_HOST
#   internal — deploy in-cluster Postfix + Dovecot; Keycloak uses Postfix
# =============================================================================

# Guard against double-sourcing.
[[ -n "${GENTIAN_MAIL_LIB_LOADED:-}" ]] && return 0
GENTIAN_MAIL_LIB_LOADED=1

# mail_mode_valid <mode> — one of the three the claim's schema takes.
mail_mode_valid() {
    [[ "$1" == "relay" || "$1" == "external" || "$1" == "internal" ]]
}

# mail_mode_sends_through_login [mode] — the cluster sends through a server
# outside it, a relay or an external mail server, with a login somebody
# supplies (the smtp-relay credential) at the address the claim names
# (mail.host). The other mode, internal, derives its own.
mail_mode_sends_through_login() {
    local mode="${1:-$(gentian_mail_service_mode)}"
    [[ "${mode}" == "relay" || "${mode}" == "external" ]]
}

# mail_mode_renamed_message <mode> — what to say about a value the claim no
# longer takes. Nothing for a value that was never one.
mail_mode_renamed_message() {
    case "$1" in
        system|kernel)
            echo "mail.serviceMode '$1' is now called 'internal'. Change it on the Cluster claim (or MAIL_SERVICE_MODE in install.env)."
            ;;
    esac
}

mail_network_mode_compatible() {
    local mode="${1:-$(gentian_mail_service_mode)}"
    local network="${2:-${NETWORK_MODE:-tunnel}}"
    [[ "${mode}" != "internal" || "${network}" != "tunnel" ]]
}

mail_network_mode_incompatibility_message() {
    cat <<'EOF'
MAIL_SERVICE_MODE=internal is incompatible with NETWORK_MODE=tunnel.
Cloudflare tunnel exposes HTTP/HTTPS only — it cannot receive inbound SMTP (ports 25/587) or act as a public MX endpoint.
Use MAIL_SERVICE_MODE=relay (or external, with a mail server outside the cluster) with EXTERNAL_SMTP_HOST + SMTP_RELAY_* credentials for invitation and outbound mail on tunnel clusters, or switch to NETWORK_MODE=static-ip when you have a reachable SMTP ingress.
EOF
}
