#!/usr/bin/env bash
# =============================================================================
# scripts/lib/mail-lib.sh — MAIL_SERVICE_MODE install/update helpers
# =============================================================================
# Sourced from scripts/lib/load.sh (and therefore install.sh / update.sh).
#
# MAIL_SERVICE_MODE:
#   external — apps (Keycloak, etc.) send mail via EXTERNAL_SMTP_HOST directly
#   kernel   — deploy in-cluster Postfix + Dovecot; Keycloak uses Postfix
# =============================================================================

# Guard against double-sourcing.
[[ -n "${GENTIAN_MAIL_LIB_LOADED:-}" ]] && return 0
GENTIAN_MAIL_LIB_LOADED=1

# The namespace the kernel mail stack runs in.
#
# platform-kernel, matching the operator's SERVICES_NAMESPACE — not gentian-<env>.
# The operator writes postfix-kernel-virtual-mailbox-maps here and hands tenant
# apps postfix-<env>.platform-kernel as their SMTP host, and a Pod can only mount
# a ConfigMap from its own namespace, so this is where Postfix has to run. Seeding
# the maps into gentian-<env> instead meant Postfix mounted a ConfigMap the
# operator never updated: mail worked for the tenants that existed at install and
# silently rejected every later one.
_mail_kernel_namespace() {
    echo "${KERNEL_NAMESPACE:-${SERVICES_NAMESPACE:-platform-kernel}}"
}

# MAIL_SERVICE_MODE=kernel needs reachable SMTP ingress; Cloudflare tunnel is HTTP-only.
mail_network_mode_compatible() {
    local mode="${1:-${MAIL_SERVICE_MODE:-external}}"
    local network="${2:-${NETWORK_MODE:-tunnel}}"
    [[ "${mode}" != "kernel" || "${network}" != "tunnel" ]]
}

mail_network_mode_incompatibility_message() {
    cat <<'EOF'
MAIL_SERVICE_MODE=kernel is incompatible with NETWORK_MODE=tunnel.
Cloudflare tunnel exposes HTTP/HTTPS only — it cannot receive inbound SMTP (ports 25/587) or act as a public MX endpoint.
Use MAIL_SERVICE_MODE=external with EXTERNAL_SMTP_HOST + SMTP_RELAY_* credentials for invitation and outbound mail on tunnel clusters, or switch to NETWORK_MODE=static-ip when you have a reachable SMTP ingress for kernel mail.
EOF
}

# =============================================================================
# Stage 1 mail delivery (external SMTP or in-cluster Postfix).
# =============================================================================
install_kernel_mail() {
    local mode="${MAIL_SERVICE_MODE:-external}"
    banner "Mail delivery (MAIL_SERVICE_MODE=${mode})"

    case "${mode}" in
        external|kernel) ;;
        *)
            error "MAIL_SERVICE_MODE must be external or kernel (got: ${mode})"
            return 1
            ;;
    esac

    KERNEL_NAMESPACE="$(_mail_kernel_namespace)"
    export KERNEL_NAMESPACE

    if ! mail_network_mode_compatible "${mode}" "${NETWORK_MODE:-tunnel}"; then
        error "$(mail_network_mode_incompatibility_message)"
        return 1
    fi

    # Neither mode configures Postfix from here any more; this step reports what
    # the declarative path will do and verifies the result.
    #
    # It used to patch three things imperatively, and all three are now rendered:
    # ALLOWED_SENDER_DOMAINS from the chart's kernelDomain, the relay from
    # relayHost/relayPort through the ApplicationSet, and the inbound maps by the
    # operator's mail reconciler. Two of those patches targeted objects Argo CD and
    # provider-helm own, so they were reverted on the next sync — the relay patch
    # was the only thing configuring an external relay, which is why external mode
    # worked until a sync and then stopped. The fourth wrote values in the previous
    # chart's schema (a top-level `postfix:` key that bokysan/mail does not have)
    # and was discarded in full.
    if [[ "${mode}" == "external" ]]; then
        if [[ -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
            warn "MAIL_SERVICE_MODE=external but EXTERNAL_SMTP_HOST is unset —" \
                 "Keycloak invite/reset emails will not send until configured."
        else
            info "Outbound mail: external SMTP at ${EXTERNAL_SMTP_HOST}:${EXTERNAL_SMTP_PORT:-587}"
            info "Postfix relays via the same host, from the chart's relayHost/relayPort."
        fi
        info "Keycloak realm SMTP will be configured during portal bootstrap."
        return 0
    fi

    info "MAIL_SERVICE_MODE=kernel — in-cluster Postfix + Dovecot via the 09-infra-helm ApplicationSet."
    info "Keycloak realm SMTP will target postfix-${ENV:-dev}.${KERNEL_NAMESPACE}.svc.cluster.local:587"
    if ! verify_dovecot_installation; then
        error "Dovecot installation verification failed."
        return 1
    fi
}
