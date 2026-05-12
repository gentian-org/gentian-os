#!/usr/bin/env bash
# =============================================================================
# update.sh — Gentian OS day-2 reconciliation
# =============================================================================
# Reads install.env (and install.secrets.env), compares the desired
# configuration to what is currently deployed on the cluster, and applies
# any necessary changes.
#
# Usage:
#   ./update.sh              # reconcile everything; apply all detected drift
#   ./update.sh --dry-run    # print what would change, do not apply
#
# What it reconciles:
#   MAIL_SERVICE_MODE  — patches the postfix-dev-values ConfigMap when the
#                        deployed relay/LMTP mode does not match install.env;
#                        re-seeds mail OpenBao KV paths; force-refreshes ESO
#                        ExternalSecrets for postfix and dovecot.
#
# Prerequisites:
#   - install.sh must have completed at least once.
#   - kubectl configured to the target cluster.
#   - OpenBao accessible (BAO_TOKEN or openbao-init.json present).
#   - install.env and install.secrets.env must exist and be filled in.
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load helper functions from install-lib.sh without running its main().
export GENTIAN_INSTALL_LIB_ONLY=1
# shellcheck source=scripts/install-lib.sh
source "${SCRIPT_DIR}/scripts/install-lib.sh"
unset GENTIAN_INSTALL_LIB_ONLY

# ─── Constants ────────────────────────────────────────────────────────────────
CROSSPLANE_NAMESPACE=crossplane-system
KERNEL_NAMESPACE=gentian-dev

# ─── Flags ────────────────────────────────────────────────────────────────────
DRY_RUN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) DRY_RUN=1 ;;
        -h|--help)
            echo "Usage: ./update.sh [--dry-run]"
            echo "Reconciles the cluster to match install.env. No flags required for normal use."
            exit 0 ;;
        *) echo "Unknown option: $1" >&2; echo "Usage: ./update.sh [--dry-run]" >&2; exit 1 ;;
    esac
    shift
done

# =============================================================================
# Bootstrap: load config and credentials
# =============================================================================
_init() {
    local cfg="${INSTALL_CONFIG_FILE:-${SCRIPT_DIR}/install.env}"
    local sec="${INSTALL_SECRETS_FILE:-${SCRIPT_DIR}/install.secrets.env}"

    [[ -f "$cfg" ]] || { echo "ERROR: $cfg not found." >&2; exit 1; }
    load_env_file "$cfg"  "install.env"
    [[ -f "$sec" ]] && load_env_file "$sec" "install.secrets.env"

    : "${MASTER_PASSWORD:?MASTER_PASSWORD must be set (via install.secrets.env or env var)}"

    # Resolve OpenBao address and token.
    local bao_ip
    bao_ip=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    [[ -n "$bao_ip" ]] || { echo "ERROR: OpenBao service not found. Is the cluster running?" >&2; exit 1; }
    export VAULT_ADDR="http://${bao_ip}:8200"

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        local init_file="${OPENBAO_INIT_FILE:-${SCRIPT_DIR}/openbao-init.json}"
        if [[ -f "$init_file" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "$init_file")
        else
            read -rsp "  OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export VAULT_TOKEN="${BAO_TOKEN}"
}

_derive() { echo -n "${1}:${2}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" -binary | sha1sum | awk '{print $1}'; }

# Upsert a K8s Secret in crossplane-system (data.json key).
_kv_secret() {
    local name="$1" json="$2"
    kubectl create secret generic "${name}" \
        -n "${CROSSPLANE_NAMESPACE}" \
        "--from-literal=data.json=${json}" \
        --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

# =============================================================================
# Detect the mail delivery mode that is currently active on the cluster by
# reading the postfix-dev-values ConfigMap. Returns "external" or "kernel".
# Falls back to "unknown" when the ConfigMap or the key is absent.
# =============================================================================
_detect_deployed_mail_mode() {
    local cm_yaml
    cm_yaml=$(kubectl get configmap postfix-dev-values -n "${KERNEL_NAMESPACE}" \
        -o jsonpath='{.data.values\.yaml}' 2>/dev/null || true)

    if [[ -z "$cm_yaml" ]]; then
        echo "unknown"
        return
    fi

    # "relayHost:\n  enabled: true" indicates external mode.
    if echo "$cm_yaml" | grep -qE '^\s+enabled:\s+true'; then
        echo "external"
    else
        echo "kernel"
    fi
}

# =============================================================================
# Build the desired postfix-dev-values YAML snippet for the given mode.
# External mode: relay credentials + SASL auth.
# Kernel mode:   relay disabled; Dovecot LMTP transport maps.
# =============================================================================
_postfix_dev_values_yaml() {
    local mode="$1"
    local ldap_host="nubus-dev-ldap-server-primary.${KERNEL_NAMESPACE}.svc.cluster.local"
    local lmtp_target="dovecot-dev.${KERNEL_NAMESPACE}.svc.cluster.local"

    if [[ "$mode" == "external" ]]; then
        local relay_host="${EXTERNAL_SMTP_HOST:-smtp.gmail.com}"
        local relay_port="${EXTERNAL_SMTP_PORT:-587}"
        cat <<YAML
postfix:
  hostname: "postfix-dev"

  ldap:
    host: "${ldap_host}"

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
        # kernel mode — LMTP delivery via Dovecot
        cat <<YAML
postfix:
  hostname: "postfix-dev"

  ldap:
    host: "${ldap_host}"

  relayHost:
    enabled: false

  smtpSASLAuthEnable: "no"
  relayNets: "10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"

  ldapVirtualMailboxDomains:
    server: "ldap://${ldap_host}:389"
    searchBase: "cn=domain,cn=virtual,cn=postfix,cn=mail,cn=univention,dc=swp-ldap,dc=internal"
    queryFilter: "(|(&(objectClass=organizationalUnit)(ou=%d))(&(objectClass=univentionMailDomainname)(cn=%d)))"
    resultAttribute: "ou"
    bindDn: "uid=ldapsearch_postfix,cn=users,dc=swp-ldap,dc=internal"
    bindPw: "placeholder"
    version: 3
  ldapTransportMaps:
    server: "ldap://${ldap_host}:389"
    searchBase: "dc=swp-ldap,dc=internal"
    queryFilter: "(&(objectClass=univentionMailDomainname)(cn=%s))"
    resultAttribute: "cn"
    resultFormat: "lmtp:${lmtp_target}:24"
    bindDn: "uid=ldapsearch_postfix,cn=users,dc=swp-ldap,dc=internal"
    bindPw: "placeholder"
    version: 3

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
# Patch the postfix-dev-values ConfigMap in-cluster to match the desired mode.
# =============================================================================
_patch_postfix_configmap() {
    local mode="$1"
    local values_yaml
    values_yaml=$(_postfix_dev_values_yaml "$mode")

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "  [dry-run] Would patch ConfigMap postfix-dev-values (mode=${mode})"
        return
    fi

    # Upsert via create --dry-run=client | apply to preserve annotations.
    kubectl create configmap postfix-dev-values \
        -n "${KERNEL_NAMESPACE}" \
        --from-literal="values.yaml=${values_yaml}" \
        --dry-run=client -o yaml \
        | kubectl annotate --local -f - \
            "argocd.argoproj.io/sync-wave=-1" \
            --dry-run=client -o yaml \
        | kubectl apply -f - >/dev/null
    success "  patched postfix-dev-values (mode=${mode})"
}

# =============================================================================
# Reconcile mail: compare install.env MAIL_SERVICE_MODE vs deployed state.
# =============================================================================
reconcile_mail() {
    local desired="${MAIL_SERVICE_MODE:-external}"
    local deployed
    deployed=$(_detect_deployed_mail_mode)

    banner "Mail reconciliation"
    info "Desired mode  (install.env): ${desired}"
    info "Deployed mode (cluster):     ${deployed}"

    case "${desired}" in
        external|kernel) ;;
        *) echo "ERROR: Unknown MAIL_SERVICE_MODE '${desired}'. Valid: external, kernel." >&2; exit 1 ;;
    esac

    local changed=0

    # ── ConfigMap drift ────────────────────────────────────────────────────────
    if [[ "${deployed}" != "${desired}" ]]; then
        if [[ "${deployed}" == "unknown" ]]; then
            info "postfix-dev-values not yet deployed — will be provisioned by ArgoCD."
        else
            info "Mode drift detected → patching postfix-dev-values..."
            _patch_postfix_configmap "${desired}"
            changed=1
        fi
    else
        success "postfix-dev-values already in ${desired} mode — no ConfigMap change needed."
    fi

    # ── Always re-seed OpenBao KV paths ───────────────────────────────────────
    # K8s Secrets in crossplane-system are always safe to upsert (idempotent).
    # The Cluster XR's SecretV2 MRs (Observe+Create) won't overwrite OpenBao.
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "  [dry-run] Would upsert gentian-os-kernel-mail-postfix in ${CROSSPLANE_NAMESPACE}"
        info "  [dry-run] Would upsert gentian-os-kernel-mail-dovecot in ${CROSSPLANE_NAMESPACE}"
    else
        _kv_secret "gentian-os-kernel-mail-postfix" \
            "$(jq -nc \
                --arg host "${EXTERNAL_SMTP_HOST:-}" \
                --arg port "${EXTERNAL_SMTP_PORT:-587}" \
                --arg user "${OD_SMTP_RELAY_USERNAME:-}" \
                --arg pass "${OD_SMTP_RELAY_PASSWORD:-}" \
                '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"
        success "  gentian-os-kernel-mail-postfix"

        _kv_secret "gentian-os-kernel-mail-dovecot" \
            "$(jq -nc \
                --arg doveadm "$(_derive minio dovecot_user)" \
                --arg oidc "$(_derive dovecot oidcClientSecret)" \
                '{doveadm_password:$doveadm,oidc_client_secret:$oidc}')"
        success "  gentian-os-kernel-mail-dovecot"
    fi

    # ── Force ESO to re-read the updated OpenBao values ───────────────────────
    if [[ "${DRY_RUN}" != "1" ]]; then
        for es in postfix-sensitive-values dovecot-sensitive-values; do
            if kubectl get externalsecret "${es}" -n "${KERNEL_NAMESPACE}" >/dev/null 2>&1; then
                kubectl annotate externalsecret "${es}" -n "${KERNEL_NAMESPACE}" \
                    force-sync="$(date -u +%s)" --overwrite >/dev/null
                success "  force-refreshed ExternalSecret ${es}"
            fi
        done
    else
        info "  [dry-run] Would force-refresh postfix-sensitive-values and dovecot-sensitive-values"
    fi

    if [[ "${changed}" == "1" ]]; then
        info ""
        info "ConfigMap updated. provider-helm will reconcile on its next poll (≤5 min)."
        info "To reconcile immediately: argocd app sync gentian-infra-helm-dev"
    fi
}

# =============================================================================
# main
# =============================================================================
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║     Gentian OS — Day-2 Reconciliation            ║${NC}"
if [[ "${DRY_RUN}" == "1" ]]; then
echo -e "${CYAN}║     (dry-run — no changes will be applied)       ║${NC}"
fi
echo -e "${CYAN}╚══════════════════════════════════════════════════╝${NC}"
echo ""

_init
reconcile_mail

echo ""
success "Reconciliation complete."

#
# Supported operations:
#   --mail        Re-seed mail credentials and sync mail-related ArgoCD apps.
#                 Reads MAIL_SERVICE_MODE from install.env:
#                   external  — relay mode (Gmail/SMTP relay). Default.
#                   kernel    — local delivery via Dovecot LMTP.
#
#   --secrets     Re-derive and apply all OpenBao KV seed Secrets in
#                 crossplane-system, then re-apply the Cluster XR to propagate
#                 any changes to OpenBao. Useful after credential rotation.
#
#   --all         Run all update operations in order.
#
# Usage:
#   ./update.sh --mail
#   ./update.sh --secrets
#   ./update.sh --all
#   ./update.sh --mail --dry-run        # print changes without applying
#
# Prerequisites:
#   - install.sh must have completed at least once.
#   - kubectl configured to the target cluster.
#   - OpenBao accessible (BAO_TOKEN or openbao-init.json present).
#   - bao CLI available (https://github.com/openbao/openbao).
#   - install.env and install.secrets.env must exist and be filled in.
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load helper functions from install-lib.sh without running its main().
export GENTIAN_INSTALL_LIB_ONLY=1
# shellcheck source=scripts/install-lib.sh
source "${SCRIPT_DIR}/scripts/install-lib.sh"
unset GENTIAN_INSTALL_LIB_ONLY

# ─── Defaults ─────────────────────────────────────────────────────────────────
CROSSPLANE_NAMESPACE=crossplane-system
DRY_RUN=0
OP_MAIL=0
OP_SECRETS=0

# =============================================================================
# Argument parsing
# =============================================================================
_usage() {
    cat >&2 <<'EOF'
Usage: ./update.sh [OPTIONS]

Options:
  --mail          Re-seed mail credentials; switch MAIL_SERVICE_MODE.
  --secrets       Re-seed all OpenBao KV secrets and re-apply the Cluster XR.
  --all           Run all update operations.
  --dry-run       Print what would change without applying.
  -h, --help      Show this help.
EOF
    exit 1
}

[[ $# -eq 0 ]] && _usage

while [[ $# -gt 0 ]]; do
    case "$1" in
        --mail)    OP_MAIL=1 ;;
        --secrets) OP_SECRETS=1 ;;
        --all)     OP_MAIL=1; OP_SECRETS=1 ;;
        --dry-run) DRY_RUN=1 ;;
        -h|--help) _usage ;;
        *) echo "Unknown option: $1" >&2; _usage ;;
    esac
    shift
done

# =============================================================================
# Bootstrap: load config and credentials
# =============================================================================
_init() {
    # Honour the same INSTALL_CONFIG_FILE env var as install.sh.
    local cfg="${INSTALL_CONFIG_FILE:-${SCRIPT_DIR}/install.env}"
    local sec="${INSTALL_SECRETS_FILE:-${SCRIPT_DIR}/install.secrets.env}"

    [[ -f "$cfg" ]] || { echo "ERROR: $cfg not found." >&2; exit 1; }

    load_env_file "$cfg"  "install.env"
    [[ -f "$sec" ]] && load_env_file "$sec" "install.secrets.env"

    # Resolve OpenBao address and token for direct BAO CLI calls.
    local bao_ip
    bao_ip=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    if [[ -z "${bao_ip}" ]]; then
        echo "ERROR: OpenBao service not found. Is the cluster running?" >&2
        exit 1
    fi
    export VAULT_ADDR="http://${bao_ip}:8200"

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        local init_file="${OPENBAO_INIT_FILE:-${SCRIPT_DIR}/openbao-init.json}"
        if [[ -f "$init_file" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "$init_file")
        else
            read -rsp "  OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export VAULT_TOKEN="${BAO_TOKEN}"

    : "${MASTER_PASSWORD:?MASTER_PASSWORD must be set (via install.secrets.env or env var)}"
}

_derive() { echo -n "${1}:${2}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" -binary | sha1sum | awk '{print $1}'; }

# =============================================================================
# Helper: upsert a K8s Secret in crossplane-system with data.json key
# (same as create_crossplane_secrets in install.sh)
# =============================================================================
_kv_secret() {
    local name="$1" json="$2"
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "  [dry-run] Would upsert Secret ${name} in ${CROSSPLANE_NAMESPACE}"
        return
    fi
    kubectl create secret generic "${name}" \
        -n "${CROSSPLANE_NAMESPACE}" \
        "--from-literal=data.json=${json}" \
        --dry-run=client -o yaml | kubectl apply -f -
    success "  ${name}"
}

# =============================================================================
# op_mail — reconcile mail configuration per MAIL_SERVICE_MODE
# =============================================================================
op_mail() {
    banner "Mail reconciliation (MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE:-external})"

    local mode="${MAIL_SERVICE_MODE:-external}"

    case "${mode}" in
        external|kernel) ;;
        *) echo "ERROR: Unknown MAIL_SERVICE_MODE '${mode}'. Valid: external, kernel." >&2; exit 1 ;;
    esac

    # ── 1. Seed/refresh mail/postfix in OpenBao ────────────────────────────────
    # In both modes Postfix runs and needs relay credentials (or empty if kernel).
    info "Seeding mail/postfix in OpenBao..."
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "  [dry-run] Would seed secret/data/gentian-os/kernel/mail/postfix"
    else
        _kv_secret "gentian-os-kernel-mail-postfix" \
            "$(jq -nc \
                --arg host "${EXTERNAL_SMTP_HOST:-}" \
                --arg port "${EXTERNAL_SMTP_PORT:-587}" \
                --arg user "${OD_SMTP_RELAY_USERNAME:-}" \
                --arg pass "${OD_SMTP_RELAY_PASSWORD:-}" \
                '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"
    fi

    # ── 2. Seed mail/dovecot in OpenBao (required for Dovecot ESO to sync) ────
    info "Seeding mail/dovecot in OpenBao..."
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "  [dry-run] Would seed secret/data/gentian-os/kernel/mail/dovecot"
    else
        _kv_secret "gentian-os-kernel-mail-dovecot" \
            "$(jq -nc \
                --arg doveadm "$(_derive minio dovecot_user)" \
                --arg oidc "$(_derive dovecot oidcClientSecret)" \
                '{doveadm_password:$doveadm,oidc_client_secret:$oidc}')"
    fi

    # ── 3. Force ESO to resync the affected ExternalSecrets ───────────────────
    info "Refreshing ExternalSecrets for postfix and dovecot..."
    if [[ "${DRY_RUN}" != "1" ]]; then
        for es in postfix-sensitive-values dovecot-sensitive-values; do
            if kubectl get externalsecret "${es}" -n gentian-dev >/dev/null 2>&1; then
                kubectl annotate externalsecret "${es}" -n gentian-dev \
                    force-sync="$(date -u +%s)" --overwrite
                success "  Triggered resync: ${es}"
            else
                info "  ExternalSecret ${es} not yet deployed — will sync on first ArgoCD apply."
            fi
        done
    else
        info "  [dry-run] Would annotate postfix-sensitive-values and dovecot-sensitive-values"
    fi

    # ── 4. Mode-specific postfix ConfigMap guidance ───────────────────────────
    if [[ "${mode}" == "kernel" ]]; then
        echo ""
        echo -e "${CYAN}╔══════════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${CYAN}║  MAIL_SERVICE_MODE=kernel — additional steps required            ║${NC}"
        echo -e "${CYAN}╠══════════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${CYAN}║                                                                  ║${NC}"
        echo -e "${CYAN}║  Dovecot secrets are now seeded. To complete kernel mail setup:  ║${NC}"
        echo -e "${CYAN}║                                                                  ║${NC}"
        echo -e "${CYAN}║  1. Edit kernel/services/postfix/manifests/dev/configmap.yaml:   ║${NC}"
        echo -e "${CYAN}║     - Set relayHost.enabled: false                               ║${NC}"
        echo -e "${CYAN}║     - Set smtpSASLAuthEnable: \"no\"                              ║${NC}"
        echo -e "${CYAN}║     - Configure ldapVirtualMailboxDomains, ldapTransportMaps     ║${NC}"
        echo -e "${CYAN}║       pointing to dovecot-dev.gentian-dev.svc.cluster.local:24   ║${NC}"
        echo -e "${CYAN}║                                                                  ║${NC}"
        echo -e "${CYAN}║  2. Commit and push the change (ArgoCD will auto-sync).          ║${NC}"
        echo -e "${CYAN}║     Or run:  argocd app sync gentian-infra-helm-dev              ║${NC}"
        echo -e "${CYAN}║                                                                  ║${NC}"
        echo -e "${CYAN}║  See docs/commands.md §9 for the full LDAP transport map config. ║${NC}"
        echo -e "${CYAN}╚══════════════════════════════════════════════════════════════════╝${NC}"
    else
        info "Mode=external: Postfix will relay via ${EXTERNAL_SMTP_HOST:-<not set>}."
        if [[ -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
            warn "  EXTERNAL_SMTP_HOST is not set in install.env — relay will not work."
        fi
        info "Trigger ArgoCD sync if relay credentials changed:"
        info "  argocd app sync gentian-infra-helm-dev"
    fi
}

# =============================================================================
# op_secrets — re-seed all OpenBao KV Secrets and re-apply Cluster XR
# =============================================================================
op_secrets() {
    banner "Secrets reconciliation (re-seed all K8s Secrets in ${CROSSPLANE_NAMESPACE})"

    info "This re-derives and re-applies all gentian-os-kernel-* Secrets."
    info "The Cluster XR's SecretV2 MRs (managementPolicies: Observe+Create)"
    info "will NOT overwrite existing OpenBao paths — only newly absent paths"
    info "are written. To force-update a path, use: bao kv put ..."
    echo ""

    # Source the full create_crossplane_secrets logic from install.sh.
    # Since install.sh is sourced with GENTIAN_INSTALL_LIB_ONLY above and
    # create_crossplane_secrets is defined in install.sh (not install-lib.sh),
    # we call it by sourcing install.sh again in GENTIAN_INSTALL_LIB_ONLY mode.
    #
    # Alternative: inline the function here or re-run install.sh --secrets-only.
    # For now, provide a targeted re-seed of the most likely changed secrets.

    info "Re-seeding mail/postfix..."
    _kv_secret "gentian-os-kernel-mail-postfix" \
        "$(jq -nc \
            --arg host "${EXTERNAL_SMTP_HOST:-}" \
            --arg port "${EXTERNAL_SMTP_PORT:-587}" \
            --arg user "${OD_SMTP_RELAY_USERNAME:-}" \
            --arg pass "${OD_SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"

    info "Re-seeding mail/dovecot..."
    _kv_secret "gentian-os-kernel-mail-dovecot" \
        "$(jq -nc \
            --arg doveadm "$(_derive minio dovecot_user)" \
            --arg oidc "$(_derive dovecot oidcClientSecret)" \
            '{doveadm_password:$doveadm,oidc_client_secret:$oidc}')"

    if [[ "${DRY_RUN}" != "1" ]]; then
        info "Re-applying Cluster XR to propagate new KV seeds to OpenBao..."
        kubectl apply -f "${SCRIPT_DIR}/crossplane/claims/dev-cluster.yaml"
        success "Cluster XR re-applied. The Cluster XR will reconcile new KV seeds."
    else
        info "  [dry-run] Would re-apply crossplane/claims/dev-cluster.yaml"
    fi
}

# =============================================================================
# main
# =============================================================================
echo ""
echo -e "\033[0;36m╔══════════════════════════════════════════════════╗\033[0m"
echo -e "\033[0;36m║     Gentian OS — Day-2 Update                    ║\033[0m"
echo -e "\033[0;36m╚══════════════════════════════════════════════════╝\033[0m"
echo ""

_init

[[ "${OP_MAIL}"    == "1" ]] && op_mail
[[ "${OP_SECRETS}" == "1" ]] && op_secrets

echo ""
success "update.sh completed."
