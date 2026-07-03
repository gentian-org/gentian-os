#!/usr/bin/env bash
# =============================================================================
# scripts/mail-lib.sh — MAIL_SERVICE_MODE install/update helpers
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

_mail_kernel_namespace() {
    echo "${KERNEL_NAMESPACE:-gentian-${ENV:-dev}}"
}

_mail_dovecot_host() {
    echo "dovecot-dev.$(_mail_kernel_namespace).svc.cluster.local"
}

# Tenant mail domains registered by the operator (platform-kernel ConfigMap).
_postfix_kernel_virtual_domains() {
    local template
    template=$(cat <<'EOF'
{{range $k,$v := .data}}{{$v}} {{end}}
EOF
)
    kubectl get configmap mail-postfix-virtual-domains -n platform-kernel \
        -o go-template="${template}" 2>/dev/null \
        | xargs echo
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
# Detect deployed Postfix mode from postfix-dev-values ConfigMap.
# Returns: external | kernel | unknown
# =============================================================================
_detect_deployed_mail_mode() {
    local ns cm_yaml
    ns="$(_mail_kernel_namespace)"
    cm_yaml=$(kubectl get configmap postfix-dev-values -n "${ns}" \
        -o jsonpath='{.data.values\.yaml}' 2>/dev/null || true)

    if [[ -z "${cm_yaml}" ]]; then
        echo "unknown"
        return
    fi

    if echo "${cm_yaml}" | grep -qE '^\s+enabled:\s+true'; then
        echo "external"
    else
        echo "kernel"
    fi
}

# =============================================================================
# Build postfix-dev-values YAML for the given mode (external relay vs kernel LMTP).
#
# Values target the public bokysan/mail chart (see kernel/services/postfix/).
# Kernel mode delivers tenant-domain mail to Dovecot via LMTP; Dovecot domain
# acceptance is still minimal (UNTESTED) — production stress tests may need more.
# =============================================================================
_postfix_dev_values_yaml() {
    local mode="$1"
    local ns
    ns="$(_mail_kernel_namespace)"

    if [[ "${mode}" == "external" ]]; then
        local relay_host="${EXTERNAL_SMTP_HOST:-smtp.gmail.com}"
        local relay_port="${EXTERNAL_SMTP_PORT:-587}"
        cat <<YAML
postfix:
  hostname: "postfix-dev"

  relayHost:
    enabled: true
    host: ${relay_host}
    port: ${relay_port}
    disableMXLookup: true
    authentication:
      enabled: true
      username:
        value: "placeholder"
      password:
        value: "placeholder"

  smtpSASLAuthEnable: "yes"
  relayNets: "10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"

resources:
  limits:
    cpu: "500m"
    memory: 256Mi
  requests:
    cpu: 50m
    memory: 64Mi
YAML
    else
        local dovecot_host domains
        dovecot_host="$(_mail_dovecot_host)"
        domains="$(_postfix_kernel_virtual_domains)"
        if [[ -z "${domains}" ]]; then
            domains="${KERNEL_DOMAIN:-gentian.local}"
        fi
        cat <<YAML
config:
  postfix:
    myhostname: postfix-dev
    virtual_transport: "lmtp:[${dovecot_host}]:24"
    virtual_mailbox_domains: "${domains}"
    virtual_mailbox_maps: "lmdb:/etc/postfix/virtual_mailbox_maps"

extraVolumes:
  - name: postfix-virtual-mailbox-maps
    configMap:
      name: postfix-kernel-virtual-mailbox-maps

extraVolumeMounts:
  - name: postfix-virtual-mailbox-maps
    mountPath: /etc/postfix/virtual_mailbox_maps
    subPath: virtual_mailbox_maps

postfix:
  hostname: "postfix-dev"
  relayHost:
    enabled: false
  smtpSASLAuthEnable: "no"
  relayNets: "10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"

resources:
  limits:
    cpu: "500m"
    memory: 256Mi
  requests:
    cpu: 50m
    memory: 64Mi
YAML
    fi
}

# Build LMTP virtual_mailbox_maps from operator-registered tenant domains.
_sync_kernel_postfix_virtual_mailbox_maps() {
    local ns domains d map_lines=""
    ns="$(_mail_kernel_namespace)"
    domains="$(_postfix_kernel_virtual_domains)"

    if [[ -z "${domains}" ]]; then
        info "  No tenant mail domains registered yet — skipping virtual_mailbox_maps"
        return 0
    fi

    for d in ${domains}; do
        map_lines+="@${d} ${d}/"$'\n'
    done

    if [[ "${DRY_RUN:-0}" == "1" ]]; then
        info "  [dry-run] Would create ConfigMap postfix-kernel-virtual-mailbox-maps"
        return 0
    fi

    kubectl create configmap postfix-kernel-virtual-mailbox-maps \
        -n "${ns}" \
        --from-literal=virtual_mailbox_maps="${map_lines}" \
        --dry-run=client -o yaml \
        | kubectl apply -f - >/dev/null
    success "  synced postfix-kernel-virtual-mailbox-maps for: ${domains}"
}

# Align the live postfix-dev pod env with the desired relay mode.
_patch_postfix_live_relay() {
    local mode="$1"
    local ns relay_host relay_port
    ns="$(_mail_kernel_namespace)"

    if ! kubectl get configmap postfix-dev -n "${ns}" >/dev/null 2>&1; then
        return 0
    fi

    if [[ "${DRY_RUN:-0}" == "1" ]]; then
        info "  [dry-run] Would patch live postfix-dev relay (mode=${mode})"
        return 0
    fi

    if [[ "${mode}" == "external" ]]; then
        relay_host="${EXTERNAL_SMTP_HOST:-smtp.gmail.com}"
        relay_port="${EXTERNAL_SMTP_PORT:-587}"
        kubectl patch configmap postfix-dev -n "${ns}" --type merge \
            -p "{\"data\":{\"RELAYHOST\":\"[${relay_host}]:${relay_port}\"}}" >/dev/null
    else
        kubectl patch configmap postfix-dev -n "${ns}" --type merge \
            -p '{"data":{"RELAYHOST":"","RELAYHOST_USERNAME":"","RELAYHOST_PASSWORD":""}}' >/dev/null
    fi
    kubectl delete pod postfix-dev-0 -n "${ns}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    success "  patched live postfix-dev relay (mode=${mode})"
}

# =============================================================================
# Ensure Postfix accepts Keycloak noreply@<KERNEL_DOMAIN> senders.
# The bokysan/mail chart builds /etc/postfix/allowed_senders from this env.
# =============================================================================
_patch_postfix_allowed_sender_domains() {
    local ns kernel_domain values_yaml
    ns="$(_mail_kernel_namespace)"
    kernel_domain="${KERNEL_DOMAIN:-}"

    if [[ -z "${kernel_domain}" ]]; then
        warn "KERNEL_DOMAIN unset — skipping Postfix ALLOWED_SENDER_DOMAINS patch"
        return 0
    fi

    if [[ "${DRY_RUN:-0}" == "1" ]]; then
        info "  [dry-run] Would patch ConfigMap postfix-base-values (ALLOWED_SENDER_DOMAINS=${kernel_domain})"
        return
    fi

    values_yaml=$(kubectl get configmap postfix-base-values -n "${ns}" \
        -o jsonpath='{.data.values\.yaml}' 2>/dev/null || true)
    if [[ -z "${values_yaml}" ]]; then
        info "postfix-base-values not yet deployed — ALLOWED_SENDER_DOMAINS will be set on first apply"
        return 0
    fi

    if echo "${values_yaml}" | grep -q "ALLOWED_SENDER_DOMAINS: \"${kernel_domain}\""; then
        success "  postfix-base-values ALLOWED_SENDER_DOMAINS already ${kernel_domain}"
        return 0
    fi

    values_yaml=$(printf '%s\n' "${values_yaml}" \
        | sed -E "s/ALLOWED_SENDER_DOMAINS: \"[^\"]*\"/ALLOWED_SENDER_DOMAINS: \"${kernel_domain}\"/")
    kubectl create configmap postfix-base-values \
        -n "${ns}" \
        --from-literal="values.yaml=${values_yaml}" \
        --dry-run=client -o yaml \
        | kubectl annotate --local -f - \
            "argocd.argoproj.io/sync-wave=-1" \
            --dry-run=client -o yaml \
        | kubectl apply -f - >/dev/null
    success "  patched postfix-base-values (ALLOWED_SENDER_DOMAINS=${kernel_domain})"

    if kubectl get configmap postfix-dev -n "${ns}" >/dev/null 2>&1; then
        kubectl patch configmap postfix-dev -n "${ns}" --type merge \
            -p "{\"data\":{\"ALLOWED_SENDER_DOMAINS\":\"${kernel_domain}\"}}" >/dev/null
        kubectl delete pod postfix-dev-0 -n "${ns}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
        success "  patched postfix-dev ConfigMap and restarted Postfix pod"
    fi
}

# =============================================================================
# Patch postfix-dev-values ConfigMap to match MAIL_SERVICE_MODE postfix layout.
# =============================================================================
_patch_postfix_configmap() {
    local mode="$1"
    local ns values_yaml
    ns="$(_mail_kernel_namespace)"
    values_yaml=$(_postfix_dev_values_yaml "${mode}")

    if [[ "${DRY_RUN:-0}" == "1" ]]; then
        info "  [dry-run] Would patch ConfigMap postfix-dev-values (mode=${mode})"
        return
    fi

    kubectl create configmap postfix-dev-values \
        -n "${ns}" \
        --from-literal="values.yaml=${values_yaml}" \
        --dry-run=client -o yaml \
        | kubectl annotate --local -f - \
            "argocd.argoproj.io/sync-wave=-1" \
            --dry-run=client -o yaml \
        | kubectl apply -f - >/dev/null
    success "  patched postfix-dev-values (mode=${mode})"
}

# =============================================================================
# Step 15b — Stage 1 mail delivery (external SMTP or in-cluster Postfix).
# =============================================================================
install_stage1_mail() {
    local mode="${MAIL_SERVICE_MODE:-external}"
    banner "Step 15b — Mail delivery (MAIL_SERVICE_MODE=${mode})"

    case "${mode}" in
        external|kernel) ;;
        *)
            error "MAIL_SERVICE_MODE must be external or kernel (got: ${mode})"
            return 1
            ;;
    esac

    export KERNEL_NAMESPACE="${KERNEL_NAMESPACE:-gentian-${ENV:-dev}}"

    if ! mail_network_mode_compatible "${mode}" "${NETWORK_MODE:-tunnel}"; then
        error "$(mail_network_mode_incompatibility_message)"
        return 1
    fi

    if [[ "${mode}" == "external" ]]; then
        if [[ -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
            warn "MAIL_SERVICE_MODE=external but EXTERNAL_SMTP_HOST is unset —" \
                 "Keycloak invite/reset emails will not send until configured."
        else
            info "Outbound mail: external SMTP at ${EXTERNAL_SMTP_HOST}:${EXTERNAL_SMTP_PORT:-587}"
            info "Keycloak uses the relay directly; in-cluster Postfix (if present) relays via the same host."
        fi
        if kubectl get namespace "$(_mail_kernel_namespace)" >/dev/null 2>&1 \
            && kubectl get configmap postfix-dev-values -n "$(_mail_kernel_namespace)" >/dev/null 2>&1; then
            _patch_postfix_configmap external
            _patch_postfix_allowed_sender_domains
            _patch_postfix_live_relay external
        fi
        info "Keycloak realm SMTP will be configured during portal bootstrap (Step 16)."
        return 0
    fi

    info "MAIL_SERVICE_MODE=kernel — deploying in-cluster Postfix + Dovecot."
    deploy_kernel_mail_services
    _sync_kernel_postfix_virtual_mailbox_maps
    _patch_postfix_configmap kernel
    _patch_postfix_allowed_sender_domains
    _patch_postfix_live_relay kernel
    info "Keycloak realm SMTP will target postfix-dev.${KERNEL_NAMESPACE}.svc.cluster.local:587"
    if ! verify_dovecot_installation; then
        error "Dovecot installation verification failed."
        return 1
    fi
}
