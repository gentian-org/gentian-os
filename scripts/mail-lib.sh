#!/usr/bin/env bash
# =============================================================================
# scripts/mail-lib.sh — MAIL_SERVICE_MODE install/update helpers
# =============================================================================
# Sourced from install-lib.sh (and therefore install.sh / update.sh).
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
# UNTESTED: values target the public bokysan/mail chart (see kernel/services/postfix/).
# scripts/mail-lib.sh still emits a minimal overlay; full LMTP→Dovecot wiring is TBD.
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
        cat <<YAML
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

    if [[ "${mode}" == "external" ]]; then
        if [[ -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
            warn "MAIL_SERVICE_MODE=external but EXTERNAL_SMTP_HOST is unset —" \
                 "Keycloak invite/reset emails will not send until configured."
        else
            info "Outbound mail: external SMTP at ${EXTERNAL_SMTP_HOST}:${EXTERNAL_SMTP_PORT:-587}"
        fi
        info "Keycloak realm SMTP will be configured during portal bootstrap (Step 16)."
        return 0
    fi

    info "MAIL_SERVICE_MODE=kernel — deploying in-cluster Postfix + Dovecot."
    deploy_kernel_mail_services
    _patch_postfix_configmap kernel
    info "Keycloak realm SMTP will target postfix-dev.${KERNEL_NAMESPACE}.svc.cluster.local:587"
}
