#!/usr/bin/env bash
# =============================================================================
# update.sh — Gentian OS day-2 reconciliation
# =============================================================================
# Reads install.env (and install.secrets.env), compares the desired
# configuration to what is currently deployed on the cluster, and applies
# any necessary changes.
#
# Usage:
#   ./update.sh --mail                     # Reconcile mail (ConfigMap, secrets, deployments)
#   ./update.sh --secrets                  # Re-seed all OpenBao KV secrets
#   ./update.sh --reconcile-releases       # Re-reconcile any failing Crossplane Release CRs
#   ./update.sh --reconcile-releases --force  # Force re-reconcile ALL Release CRs
#   ./update.sh --all                      # Run all update operations
#   ./update.sh --mail --dry-run           # Print what would change without applying
#
# What it reconciles:
#   --mail:
#     - Patches postfix-dev-values ConfigMap when MAIL_SERVICE_MODE drifts
#     - Re-seeds mail OpenBao KV paths (postfix + dovecot)
#     - Force-refreshes ESO ExternalSecrets for postfix and dovecot
#     - When MAIL_SERVICE_MODE=kernel: applies kernel mail service manifests
#       (dovecot Release CR, ConfigMaps, ExternalSecrets) via the same function
#       used by install.sh (deploy_kernel_mail_services)
#   --secrets:
#     - Re-derives and re-applies all kernel Secrets in crossplane-system
#     - Re-applies the Cluster XR to propagate new KV seeds to OpenBao
#   --reconcile-releases:
#     - Scans all kernel/services/*/manifests/${env}/release.yaml files
#     - For each Crossplane Release CR that is not Ready+Synced: deletes and
#       recreates it (provider-helm does not watch ConfigMaps, so this is the
#       only way to force value pick-up after a ConfigMap change)
#     - With --force: also re-reconciles currently healthy Release CRs
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

# ─── Constants ────────────────────────────────────────────────────────────────
CROSSPLANE_NAMESPACE=crossplane-system
KERNEL_NAMESPACE=gentian-dev

# ─── Defaults ─────────────────────────────────────────────────────────────────
DRY_RUN=0
OP_MAIL=0
OP_SECRETS=0
OP_RECONCILE=0
OP_NUBUS_RECOVER=0
OP_LDAP_ACL=0
OP_KEYCLOAK_SYNC=0
OP_FIX_KERNEL_LDAP_SCOPE=0
OP_CROSSPLANE=0
OP_APPPROFILES=0
OP_PLUGIN=0
OP_ACME_ISSUERS=0
FORCE_RECONCILE=0

# =============================================================================
# Argument parsing
# =============================================================================
_usage() {
    cat >&2 <<'EOF'
Usage: ./update.sh [OPTIONS]

Options:
  --mail                   Reconcile mail: patch postfix ConfigMap, re-seed
                           credentials, deploy kernel mail services if needed.
  --secrets                Re-seed all OpenBao KV secrets and re-apply the
                           Cluster XR.
  --reconcile-releases     Re-reconcile any Crossplane Release CR that is not
                           Ready+Synced (delete + recreate to pick up ConfigMap
                           or Secret changes that provider-helm missed).
  --force                  Modifier for --reconcile-releases: also re-reconcile
                           currently healthy Release CRs (forces value pick-up
                           after a ConfigMap change on a working release).
  --nubus-recover          Recover a stuck nubus installation: reapply the
                           stack-data-ums job in the correct namespace when the
                           done marker is absent and register-consumers is stuck.
  --ldap-acl               Update the LDAP ACL configmap from source and restart
                           the LDAP primary pod to apply the latest ACL patches.
                           Also triggers a Keycloak LDAP full sync so users are
                           re-imported with the correct enabled state.
  --keycloak-sync          Trigger a full Keycloak LDAP sync in the kernel realm
                           to re-import all users with up-to-date LDAP attributes.
                           Run this after provisioning new tenants so their admin
                           accounts are immediately visible in the portal.
  --fix-kernel-ldap-scope  Patch the kernel realm's LDAP federation scope so it
                           targets only cn=users (service accounts) instead of
                           the full tree. Run once after cluster install to fix
                           the Nubus keycloak-bootstrap default. Idempotent.
  --crossplane             Re-apply Crossplane XRDs and Compositions from the
                           repository. Run after committing composition changes
                           so the cluster picks them up without a full reinstall.
  --appprofiles            Ensure the gentian-appprofiles ArgoCD Application
                           exists so AppProfile CRs are kept in sync from the
                           gentian-apps repository.
  --plugin                 Reinstall the kubectl-gentian plugin from this
                           repository (idempotent: skips if already up-to-date).
  --acme-issuers           Re-apply cert-manager ClusterIssuers from install.env
                           (ACME_ENV=production or staging). Not included in --all.
  --all                    Run all update operations (default when no options).
  --dry-run                Print what would change without applying.
  -h, --help               Show this help.
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --mail)                OP_MAIL=1 ;;
        --secrets)             OP_SECRETS=1 ;;
        --reconcile-releases)  OP_RECONCILE=1 ;;
        --force)               FORCE_RECONCILE=1 ;;
        --nubus-recover)       OP_NUBUS_RECOVER=1 ;;
        --ldap-acl)            OP_LDAP_ACL=1 ;;
        --keycloak-sync)       OP_KEYCLOAK_SYNC=1 ;;
        --fix-kernel-ldap-scope) OP_FIX_KERNEL_LDAP_SCOPE=1 ;;
        --crossplane)          OP_CROSSPLANE=1 ;;
        --appprofiles)         OP_APPPROFILES=1 ;;
        --plugin)              OP_PLUGIN=1 ;;
        --acme-issuers)        OP_ACME_ISSUERS=1 ;;
        --all)                 OP_MAIL=1; OP_SECRETS=1; OP_RECONCILE=1; OP_LDAP_ACL=1; OP_CROSSPLANE=1; OP_APPPROFILES=1; OP_PLUGIN=1 ;;
        --dry-run)             DRY_RUN=1 ;;
        -h|--help)             _usage ;;
        *) echo "Unknown option: $1" >&2; _usage ;;
    esac
    shift
done

# Default: reconcile everything when no specific operation is requested.
if [[ "${OP_MAIL}" == "0" && "${OP_SECRETS}" == "0" && "${OP_RECONCILE}" == "0" && "${OP_NUBUS_RECOVER}" == "0" && "${OP_LDAP_ACL}" == "0" && "${OP_KEYCLOAK_SYNC}" == "0" && "${OP_FIX_KERNEL_LDAP_SCOPE}" == "0" && "${OP_CROSSPLANE}" == "0" && "${OP_APPPROFILES}" == "0" && "${OP_PLUGIN}" == "0" ]]; then
    OP_MAIL=1
    OP_SECRETS=1
    OP_RECONCILE=1
    OP_LDAP_ACL=1
    OP_CROSSPLANE=1
    OP_APPPROFILES=1
    OP_PLUGIN=1
fi

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

# =============================================================================
# Helper: upsert a K8s Secret in crossplane-system with data.json key.
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
        --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    success "  ${name}"
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
# op_mail — reconcile mail configuration per MAIL_SERVICE_MODE
#
# Combines:
#   - ConfigMap drift detection + patching (from old reconcile_mail)
#   - OpenBao KV secret seeding (postfix + dovecot)
#   - ESO ExternalSecret force-resync
#   - Kernel mail service deployment when mode=kernel
#     (calls deploy_kernel_mail_services() from install.sh)
# =============================================================================
op_mail() {
    banner "Mail reconciliation (MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE:-external})"

    local mode="${MAIL_SERVICE_MODE:-external}"

    case "${mode}" in
        external|kernel) ;;
        *) echo "ERROR: Unknown MAIL_SERVICE_MODE '${mode}'. Valid: external, kernel." >&2; exit 1 ;;
    esac

    # ── 1. Detect ConfigMap drift and patch if needed ─────────────────────────
    local deployed
    deployed=$(_detect_deployed_mail_mode)
    info "Desired mode  (install.env): ${mode}"
    info "Deployed mode (cluster):     ${deployed}"

    if [[ "${deployed}" != "${mode}" ]]; then
        if [[ "${deployed}" == "unknown" ]]; then
            info "postfix-dev-values not yet deployed — will be provisioned by ArgoCD."
        else
            info "Mode drift detected → patching postfix-dev-values..."
            _patch_postfix_configmap "${mode}"
            info ""
            info "ConfigMap updated. provider-helm will reconcile on its next poll (≤5 min)."
            info "To reconcile immediately: argocd app sync gentian-infra-helm-dev"
        fi
    else
        success "postfix-dev-values already in ${mode} mode — no ConfigMap change needed."
    fi

    # ── 2. Seed/refresh mail/postfix in OpenBao ────────────────────────────────
    info "Seeding mail/postfix credentials in crossplane-system..."
    _kv_secret "gentian-os-kernel-mail-postfix" \
        "$(jq -nc \
            --arg host "${EXTERNAL_SMTP_HOST:-}" \
            --arg port "${EXTERNAL_SMTP_PORT:-587}" \
            --arg user "${OD_SMTP_RELAY_USERNAME:-}" \
            --arg pass "${OD_SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"

    # ── 3. Seed mail/dovecot in OpenBao (required for Dovecot ESO to sync) ────
    info "Seeding mail/dovecot credentials in crossplane-system..."
    _kv_secret "gentian-os-kernel-mail-dovecot" \
        "$(jq -nc \
            --arg doveadm "$(_derive minio dovecot_user)" \
            --arg oidc "$(_derive dovecot oidcClientSecret)" \
            '{doveadm_password:$doveadm,oidc_client_secret:$oidc}')"

    # ── 4. Force ESO to resync the affected ExternalSecrets ───────────────────
    info "Refreshing ExternalSecrets for postfix and dovecot..."
    if [[ "${DRY_RUN}" != "1" ]]; then
        for es in postfix-sensitive-values dovecot-sensitive-values; do
            if kubectl get externalsecret "${es}" -n "${KERNEL_NAMESPACE}" >/dev/null 2>&1; then
                kubectl annotate externalsecret "${es}" -n "${KERNEL_NAMESPACE}" \
                    force-sync="$(date -u +%s)" --overwrite >/dev/null
                success "  Triggered resync: ${es}"
            else
                info "  ExternalSecret ${es} not yet deployed — will sync on first ArgoCD apply."
            fi
        done
    else
        info "  [dry-run] Would annotate postfix-sensitive-values and dovecot-sensitive-values"
    fi

    # ── 5. When mode=kernel, ensure kernel mail services are deployed ─────────
    # deploy_kernel_mail_services() is sourced from install.sh; it is idempotent
    # (kubectl apply) so safe to call on every --mail run when mode=kernel.
    if [[ "${mode}" == "kernel" ]]; then
        deploy_kernel_mail_services
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

    # Re-seed mail credentials (the two most commonly rotated paths).
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
# op_reconcile_releases — delete + recreate unhealthy (or all) Release CRs
#
# Crossplane provider-helm does NOT watch ConfigMaps or Secrets referenced via
# valuesFrom.  When a ConfigMap changes after the initial install, the Release
# CR must be deleted and recreated to pick up the new values.
#
# This function:
#   1. Finds every kernel/services/*/manifests/${env}/release.yaml
#   2. Extracts the names of all Release CRs defined in each file
#   3. Checks each Release for Ready=True and Synced=True
#   4. For any that fail the check (or all, when FORCE_RECONCILE=1):
#      a. Re-applies the manifest directory (ConfigMaps + ExternalSecrets first)
#      b. Deletes the Release CR and waits up to 30 s for it to disappear
#      c. Re-applies the manifest directory to recreate only the missing Release
# =============================================================================
op_reconcile_releases() {
    local env="${KERNEL_NAMESPACE#gentian-}"

    if [[ "${FORCE_RECONCILE}" == "1" ]]; then
        banner "Helm Release re-reconciliation — FORCE (env=${env})"
    else
        banner "Helm Release re-reconciliation — check failing (env=${env})"
    fi

    # ── Collect release.yaml files ────────────────────────────────────────────
    local manifest_files=()
    while IFS= read -r -d '' f; do
        manifest_files+=("$f")
    done < <(find "${SCRIPT_DIR}/kernel/services" \
        -name "release.yaml" \
        -path "*/${env}/*" \
        -print0 | sort -z)

    if [[ ${#manifest_files[@]} -eq 0 ]]; then
        info "No release.yaml files found under kernel/services/*/${env}/"
        return
    fi

    local any_action=0

    for release_file in "${manifest_files[@]}"; do
        local manifest_dir
        manifest_dir="$(dirname "${release_file}")"

        # Extract Release CR names (kind: Release blocks only, metadata.name at
        # 2-space indent — deeper name: fields are inside spec and are ignored).
        local names=()
        while IFS= read -r name; do
            [[ -n "$name" ]] && names+=("$name")
        done < <(awk '
            /^kind: Release/ { in_release=1 }
            in_release && /^  name:/ { print $2; in_release=0 }
            /^---/ { in_release=0 }
        ' "${release_file}")

        [[ ${#names[@]} -eq 0 ]] && continue

        for name in "${names[@]}"; do
            # ── Missing release ────────────────────────────────────────────────
            if ! kubectl get release.helm.crossplane.io/"${name}" \
                    >/dev/null 2>&1; then
                warn "  ${name}: not found — applying manifest directory"
                if [[ "${DRY_RUN}" != "1" ]]; then
                    kubectl apply -f "${manifest_dir}/" >/dev/null
                    any_action=1
                else
                    info "  [dry-run] Would: kubectl apply -f ${manifest_dir}/"
                fi
                continue
            fi

            # ── Check current status ───────────────────────────────────────────
            local ready synced message
            ready=$(kubectl get release.helm.crossplane.io/"${name}" \
                -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' \
                2>/dev/null || echo "Unknown")
            synced=$(kubectl get release.helm.crossplane.io/"${name}" \
                -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}' \
                2>/dev/null || echo "Unknown")
            message=$(kubectl get release.helm.crossplane.io/"${name}" \
                -o jsonpath='{.status.conditions[?(@.type=="Synced")].message}' \
                2>/dev/null || echo "")

            local needs_reconcile=0
            if [[ "${ready}" != "True" || "${synced}" != "True" ]]; then
                needs_reconcile=1
                warn "  ${name}: READY=${ready} SYNCED=${synced} — unhealthy"
                [[ -n "$message" ]] && warn "    ↳ ${message:0:120}"
            elif [[ "${FORCE_RECONCILE}" == "1" ]]; then
                needs_reconcile=1
                info "  ${name}: READY=${ready} SYNCED=${synced} — forcing re-reconcile"
            else
                success "  ${name}: READY=${ready} SYNCED=${synced} — OK"
            fi

            [[ "${needs_reconcile}" == "0" ]] && continue

            # ── Re-reconcile ───────────────────────────────────────────────────
            if [[ "${DRY_RUN}" == "1" ]]; then
                info "  [dry-run] Would delete and recreate: ${name}"
                continue
            fi

            # Apply manifest dir first so ConfigMaps/ExternalSecrets are current.
            info "  Applying manifest directory (ConfigMaps / ExternalSecrets)..."
            # Exclude kustomization.yaml — kubectl apply -f dir/ would try to
            # process it as a plain resource, failing with "no matches for kind".
            while IFS= read -r -d '' f; do
                kubectl apply -f "${f}" >/dev/null
            done < <(find "${manifest_dir}" -maxdepth 1 -name '*.yaml' \
                ! -name 'kustomization.yaml' -print0 | sort -z)

            info "  Deleting ${name}..."
            kubectl delete release.helm.crossplane.io/"${name}" \
                --ignore-not-found >/dev/null

            # Wait up to 30 s for the resource to disappear.
            local i=0
            while kubectl get release.helm.crossplane.io/"${name}" \
                    >/dev/null 2>&1; do
                sleep 1
                (( ++i ))
                if [[ $i -ge 30 ]]; then
                    warn "  Timed out waiting for ${name} deletion — skipping"
                    break
                fi
            done

            info "  Recreating ${name}..."
            kubectl apply -f "${manifest_dir}/" >/dev/null
            success "  ${name}: re-reconciled"
            any_action=1
        done
    done

    echo ""
    if [[ "${DRY_RUN}" != "1" ]]; then
        if [[ "${any_action}" == "1" ]]; then
            info "Re-reconciliation triggered. Monitor with:"
            info "  kubectl get release.helm.crossplane.io"
        else
            success "All Release CRs are healthy — no re-reconciliation needed."
        fi
    fi
}

# =============================================================================
# _trigger_keycloak_ldap_sync — trigger a full LDAP sync in Keycloak's
#                               opendesk realm so all users (including newly
#                               provisioned tenant admins) are re-imported with
#                               the correct enabled state.
#
# Background: Keycloak's opendesk realm uses LDAP federation with
# importEnabled=true but no automatic sync period (fullSyncPeriod=-1).  When
# an LDAP user is first imported on login, a brief race during UDM user
# creation can set shadowExpire=1, causing the univention-ldap-mapper to mark
# the account as disabled (isAccountDisabled() returns true only when
# shadowExpire==1).  A fresh full sync re-reads LDAP with the correct
# (non-expired) attributes and updates the local Keycloak user record.
# =============================================================================
_trigger_keycloak_ldap_sync() {
    local release_name="nubus-dev"
    local ns="${KERNEL_NAMESPACE}"
    local kc_realm="${KERNEL_REALM:-kernel}"

    info "Triggering Keycloak LDAP full sync for realm '${kc_realm}'..."

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would trigger Keycloak LDAP full sync"
        return 0
    fi

    # Get Keycloak pod IP (headless service — use pod IP directly).
    local kc_pod_ip
    kc_pod_ip=$(kubectl get pod "${release_name}-keycloak-0" -n "${ns}" \
        -o jsonpath='{.status.podIP}' 2>/dev/null || true)
    if [[ -z "${kc_pod_ip}" ]]; then
        warn "Keycloak pod not found in ${ns} — skipping LDAP sync"
        return 0
    fi

    # Get Keycloak admin password.
    local kc_admin_pass
    kc_admin_pass=$(kubectl get secret "${release_name}-keycloak-credentials" \
        -n "${ns}" -o jsonpath='{.data.adminPassword}' 2>/dev/null | base64 -d || true)
    if [[ -z "${kc_admin_pass}" ]]; then
        warn "Keycloak admin credentials not found — skipping LDAP sync"
        return 0
    fi

    # Obtain an admin CLI token.  Username is 'kcadmin' per Nubus chart defaults.
    local kc_token
    kc_token=$(curl -sf --max-time 30 \
        "http://${kc_pod_ip}:8080/realms/master/protocol/openid-connect/token" \
        -d "grant_type=password&client_id=admin-cli&username=kcadmin&password=${kc_admin_pass}" \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null || true)
    if [[ -z "${kc_token}" ]]; then
        warn "Could not obtain Keycloak admin token — skipping LDAP sync"
        return 0
    fi

    # Discover the LDAP federation provider ID in the opendesk realm.
    local provider_id
    provider_id=$(curl -sf --max-time 30 \
        -H "Authorization: Bearer ${kc_token}" \
        "http://${kc_pod_ip}:8080/admin/realms/${kc_realm}/components?type=org.keycloak.storage.UserStorageProvider" \
        | python3 -c "
import sys, json
for p in json.load(sys.stdin):
    if p.get('providerId') == 'ldap':
        print(p['id'])
        break
" 2>/dev/null || true)
    if [[ -z "${provider_id}" ]]; then
        warn "LDAP provider not found in Keycloak '${kc_realm}' realm — skipping sync"
        return 0
    fi

    # Trigger a full user sync (re-imports all LDAP users with fresh attributes).
    local sync_result
    sync_result=$(curl -sf --max-time 120 \
        -X POST \
        -H "Authorization: Bearer ${kc_token}" \
        "http://${kc_pod_ip}:8080/admin/realms/${kc_realm}/user-storage/${provider_id}/sync?action=triggerFullSync" \
        | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('status', d))" 2>/dev/null || true)
    if [[ -n "${sync_result}" ]]; then
        success "Keycloak LDAP sync complete: ${sync_result}"
    else
        warn "Keycloak LDAP sync request sent but no result returned — check Keycloak logs"
    fi
}

# =============================================================================
# op_keycloak_sync — standalone wrapper around _trigger_keycloak_ldap_sync
# =============================================================================
op_keycloak_sync() {
    banner "Keycloak LDAP sync (realm: ${KERNEL_REALM:-kernel})"
    _trigger_keycloak_ldap_sync
}

# =============================================================================
# op_fix_kernel_ldap_scope — patch the kernel realm's LDAP User Storage
#                             Provider so it targets only the service accounts
#                             container (cn=users) rather than the full tree.
#
# Background: Nubus keycloak-bootstrap seeds the kernel realm's LDAP federation
# with usersDn=dc=swp-ldap,dc=internal (full tree, searchScope=2). This causes
# all tenant users to appear in the kernel realm, making UMC expose all tenants
# to every authenticated user. The fix scopes the kernel LDAP federation to
# cn=users,dc=swp-ldap,dc=internal (one-level, searchScope=1) so only kernel
# service accounts are imported — no human tenant users.
#
# Run once after cluster install / reinstall before provisioning tenants.
# =============================================================================
_fix_kernel_ldap_scope() {
    local release_name="nubus-dev"
    local ns="${KERNEL_NAMESPACE}"
    local kc_realm="${KERNEL_REALM:-kernel}"
    local target_users_dn="cn=users,${LDAP_BASE:-dc=swp-ldap,dc=internal}"

    info "Patching kernel realm LDAP federation scope to ${target_users_dn} (searchScope=1)..."

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would patch kernel realm LDAP federation usersDn=${target_users_dn} searchScope=1"
        return 0
    fi

    # Get Keycloak pod IP.
    local kc_pod_ip
    kc_pod_ip=$(kubectl get pod "${release_name}-keycloak-0" -n "${ns}" \
        -o jsonpath='{.status.podIP}' 2>/dev/null || true)
    if [[ -z "${kc_pod_ip}" ]]; then
        warn "Keycloak pod not found in ${ns} — skipping kernel LDAP scope fix"
        return 0
    fi

    # Get Keycloak admin password from the UMS administrator secret (reliable source).
    local kc_admin_pass
    kc_admin_pass=$(kubectl get secret "${release_name}-stack-data-ums-administrator" \
        -n "${ns}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d || true)
    if [[ -z "${kc_admin_pass}" ]]; then
        warn "UMS administrator secret not found — skipping kernel LDAP scope fix"
        return 0
    fi

    # Obtain an admin CLI token using the Administrator account (matches buildKCLDAPSyncScript).
    local kc_token
    kc_token=$(curl -sf --max-time 30 \
        "http://${kc_pod_ip}:8080/realms/master/protocol/openid-connect/token" \
        -d "grant_type=password&client_id=admin-cli&username=Administrator&password=${kc_admin_pass}" \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null || true)
    if [[ -z "${kc_token}" ]]; then
        warn "Could not obtain Keycloak admin token — skipping kernel LDAP scope fix"
        return 0
    fi

    # Find the LDAP User Storage Provider in the kernel realm.
    # Retry: keycloak-bootstrap creates the federation component asynchronously
    # and may still be running when install.sh calls this function. Wait up to
    # 5 minutes (10 × 30 s) for the provider to appear.
    local provider_json provider_id _attempt
    _attempt=0
    while true; do
        provider_json=$(curl -sf --max-time 30 \
            -H "Authorization: Bearer ${kc_token}" \
            "http://${kc_pod_ip}:8080/admin/realms/${kc_realm}/components?type=org.keycloak.storage.UserStorageProvider" \
            | python3 -c "
import sys, json
for p in json.load(sys.stdin):
    if p.get('providerId') == 'ldap':
        print(json.dumps(p))
        break
" 2>/dev/null || true)
        [[ -n "${provider_json}" ]] && break
        _attempt=$(( _attempt + 1 ))
        if (( _attempt >= 10 )); then
            warn "LDAP provider not found in realm '${kc_realm}' after 5m — skipping"
            return 0
        fi
        info "  LDAP provider not yet in realm '${kc_realm}' (attempt ${_attempt}/10) — keycloak-bootstrap still running; retry in 30s..."
        sleep 30
        # Refresh the admin token; it may expire during a long wait.
        kc_token=$(curl -sf --max-time 30 \
            "http://${kc_pod_ip}:8080/realms/master/protocol/openid-connect/token" \
            -d "grant_type=password&client_id=admin-cli&username=Administrator&password=${kc_admin_pass}" \
            | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null || true)
        [[ -n "${kc_token}" ]] || { warn "Lost Keycloak admin token during wait — skipping"; return 0; }
    done
    provider_id=$(echo "${provider_json}" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)

    # Check current usersDn — skip if already scoped correctly.
    local current_dn
    current_dn=$(echo "${provider_json}" | python3 -c "
import sys, json
p = json.load(sys.stdin)
print(p.get('config', {}).get('usersDn', [''])[0])
" 2>/dev/null || true)
    if [[ "${current_dn}" == "${target_users_dn}" ]]; then
        success "Kernel LDAP scope already correct (${target_users_dn}) — nothing to do"
        return 0
    fi
    info "Current kernel LDAP usersDn: ${current_dn} → patching to ${target_users_dn}"

    # Build the patched component JSON by updating only the relevant config fields.
    local patched_json
    patched_json=$(echo "${provider_json}" | python3 -c "
import sys, json
p = json.load(sys.stdin)
p['config']['usersDn'] = ['${target_users_dn}']
p['config']['searchScope'] = ['1']
print(json.dumps(p))
")

    # PUT the updated component back.
    local http_status
    http_status=$(curl -sf --max-time 30 -o /dev/null -w "%{http_code}" \
        -X PUT \
        -H "Authorization: Bearer ${kc_token}" \
        -H "Content-Type: application/json" \
        "http://${kc_pod_ip}:8080/admin/realms/${kc_realm}/components/${provider_id}" \
        -d "${patched_json}" 2>/dev/null || echo "ERR")
    if [[ "${http_status}" == "204" ]]; then
        success "Kernel LDAP scope patched: usersDn=${target_users_dn}, searchScope=1"
    else
        warn "Unexpected HTTP ${http_status} when patching kernel LDAP provider — check Keycloak logs"
        return 1
    fi
}

op_fix_kernel_ldap_scope() {
    banner "Fix kernel realm LDAP scope"
    _fix_kernel_ldap_scope
}

# =============================================================================
# op_ldap_acl_upgrade — refresh the LDAP ACL configmap and restart the LDAP
#                        primary pod so the latest ACL patches are applied.
#
# Background: The 92-gentian-tenant-acl.sh script is mounted from the
# nubus-dev-ldap-gentian-acl ConfigMap into the LDAP primary container's
# /entrypoint.d/ and runs at pod startup to patch slapd.conf.  When the
# script is updated (e.g. to fix mail/domain visibility in patch 10), the
# ConfigMap must be re-applied and the pod restarted for the new ACL rules
# to take effect.
#
# This function:
#   1. Re-applies the ConfigMap from the source file (idempotent).
#   2. Restarts the LDAP primary StatefulSet pod.
#   3. Waits for the pod to become Ready again.
#   4. Triggers a Keycloak LDAP full sync so any users imported during the
#      previous (potentially broken) LDAP ACL state are re-evaluated.
# =============================================================================
op_ldap_acl_upgrade() {
    local release_name="nubus-dev"
    local ns="${KERNEL_NAMESPACE}"
    local sts="${release_name}-ldap-server-primary"

    info "Updating ${release_name}-ldap-gentian-acl ConfigMap in ${ns}..."
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would apply: ${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/patches/92-gentian-tenant-acl.sh"
        return 0
    fi

    kubectl create configmap "${release_name}-ldap-gentian-acl" \
        -n "${ns}" \
        --from-file=92-gentian-tenant-acl.sh="${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/patches/92-gentian-tenant-acl.sh" \
        --dry-run=client -o yaml | kubectl apply -f -

    info "Restarting LDAP primary StatefulSet (${sts}) to apply new ACL patches..."
    kubectl rollout restart statefulset/"${sts}" -n "${ns}"

    info "Waiting for LDAP primary pod to be Ready..."
    if ! kubectl rollout status statefulset/"${sts}" -n "${ns}" --timeout=120s; then
        warn "LDAP primary pod did not become Ready within 120s — check: kubectl get pods -n ${ns}"
        return 1
    fi

    success "LDAP ACL upgrade complete."

    # ── Restart Dovecot so it reconnects to LDAP with the new ACL rules ──────
    # Dovecot caches LDAP connections. After the LDAP pod restarts with new
    # ACL patches, Dovecot must restart to re-establish connections and pick
    # up the updated ACL state (e.g. newly allowed ldapsearch_dovecot binds).
    local dovecot_dep="dovecot-dev"
    if kubectl get deployment/"${dovecot_dep}" -n "${ns}" >/dev/null 2>&1; then
        info "Restarting Dovecot (${dovecot_dep}) to reconnect to LDAP..."
        kubectl rollout restart deployment/"${dovecot_dep}" -n "${ns}"
        if ! kubectl rollout status deployment/"${dovecot_dep}" -n "${ns}" --timeout=120s; then
            warn "Dovecot did not become Ready within 120s — check: kubectl get pods -n ${ns}"
        else
            success "Dovecot restarted."
        fi
    else
        info "Dovecot deployment (${dovecot_dep}) not found in ${ns} — skipping restart."
    fi

    _trigger_keycloak_ldap_sync
}

# =============================================================================
# op_acme_issuers — re-apply ClusterIssuers per ACME_ENV in install.env
# =============================================================================
op_acme_issuers() {
    banner "cert-manager ClusterIssuers (ACME_ENV=${ACME_ENV:-production})"

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would apply: $(gentian_cluster_issuers_manifest)"
        return 0
    fi

    apply_gentian_cluster_issuers
    success "ClusterIssuers applied (ACME_ENV=${ACME_ENV:-production})."
    info "Tenant operator issuer: set tenantDNS01ClusterIssuer in Helm values to match."
    info "  production: letsencrypt-dns01-cloudflare"
    info "  staging:    letsencrypt-staging-dns01-cloudflare"
}

# =============================================================================
# op_crossplane_update — re-apply Crossplane XRDs and Compositions from repo
#
# This brings the cluster in sync with repository changes to compositions or
# XRDs without requiring a full reinstall.  Safe to run repeatedly (idempotent
# kubectl apply).
# =============================================================================
op_crossplane_update() {
    banner "Crossplane XRD and Composition update"

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would apply: crossplane/xrds/ and crossplane/compositions/"
        return 0
    fi

    info "Applying XRDs..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/xrds/cluster.yaml"
    kubectl apply -f "${SCRIPT_DIR}/crossplane/xrds/app.yaml"
    kubectl apply -f "${SCRIPT_DIR}/crossplane/xrds/tenant.yaml"

    info "Applying Compositions (all crossplane/compositions/*.yaml)..."
    apply_crossplane_platform_compositions_update

    upsert_gentian_cluster_config

    success "Crossplane XRDs and Compositions updated."
}

# =============================================================================
# op_appprofiles_bootstrap — ensure the gentian-appprofiles ArgoCD Application
#                            exists so AppProfile CRs are kept in sync from the
#                            gentian-apps repository.
#
# AppProfile CRs carry a gentianos.io/profile-name label that the app-default
# composition uses to look up profiles via function-extra-resources Selector.
# Without this ArgoCD Application the profiles are not deployed (or are missing
# the label) and all App/XApp composites fail with "AppProfile not found".
#
# This is idempotent: kubectl apply is a no-op if the Application already
# exists with identical spec.
# =============================================================================
op_appprofiles_bootstrap() {
    banner "gentian-appprofiles ArgoCD Application bootstrap"

    local repo="${GENTIAN_APPS_REPO:-https://github.com/gentian-org/gentian-apps}"
    local branch="${GENTIAN_APPS_BRANCH:-main}"
    local tmpl="${SCRIPT_DIR}/kernel/bootstrap/appprofiles-application.yaml.tmpl"

    if [[ ! -f "${tmpl}" ]]; then
        warn "Template not found: ${tmpl} — skipping appprofiles bootstrap."
        return 0
    fi

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would apply gentian-appprofiles Application (repo=${repo}, branch=${branch})"
        return 0
    fi

    info "Applying gentian-appprofiles Application (repo=${repo}, branch=${branch})..."
    sed -e "s|%REPO_URL%|${repo}|g" \
        -e "s|%BRANCH%|${branch}|g" \
        "${tmpl}" | kubectl apply -f -

    success "gentian-appprofiles Application applied."

    # Trigger an immediate ArgoCD refresh so AppProfile CRs with the updated
    # labels are synced without waiting for the automated poll interval (~3 min).
    kubectl annotate application gentian-appprofiles -n argocd \
        "argocd.argoproj.io/refresh=hard" --overwrite >/dev/null 2>&1 || true

    info "  Monitor: kubectl get application gentian-appprofiles -n argocd"
}

# =============================================================================
# op_nubus_recover — reapply the stack-data-ums job in the correct namespace
#
# Background: install.sh's _wait_and_fix_stack_data_ums() calls `helm get
# manifest | kubectl apply -f -` without an explicit -n flag.  When the
# kubectl context has a non-gentian-dev default namespace (e.g. argocd), the
# reapplied job silently lands in the wrong namespace, never creates the
# cn=stack-data-ums.done LDAP marker, and leaves register-consumers stuck in
# Init:1/2 (wait-for-data-loader) forever.
#
# This function:
#   1. Checks whether register-consumers has already completed (noop if so).
#   2. Deletes any existing failed/stuck stack-data-ums job in the target ns.
#   3. Extracts the job manifest from the Helm release and applies it with an
#      explicit -n flag so it lands in KERNEL_NAMESPACE regardless of the
#      kubectl context default namespace.
#   4. Waits up to 10 minutes for the job to complete.
# =============================================================================
op_nubus_recover() {
    local release_name="nubus-dev"
    local ns="${KERNEL_NAMESPACE}"

    banner "Nubus: stack-data-ums recovery (ns=${ns})"

    # ── 1. Check if register-consumers is already complete ────────────────────
    local rc_job
    rc_job=$(kubectl get jobs -n "${ns}" --no-headers \
        -o custom-columns=NAME:.metadata.name 2>/dev/null \
        | grep "^${release_name}-provisioning-register-consumers-" | tail -1 || true)

    if [[ -z "$rc_job" ]]; then
        info "No register-consumers job found — nubus may not be installed yet."
        return 0
    fi

    local rc_complete
    rc_complete=$(kubectl get job "${rc_job}" -n "${ns}" \
        -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' \
        2>/dev/null || echo "")

    if [[ "${rc_complete}" == "True" ]]; then
        success "register-consumers already completed — done marker is present, nothing to do."
        return 0
    fi

    info "register-consumers job '${rc_job}' is not complete — proceeding with recovery."

    # ── 2. Delete any existing failed/stuck stack-data-ums job ───────────────
    local sdu_job
    sdu_job=$(kubectl get jobs -n "${ns}" --no-headers \
        -o custom-columns=NAME:.metadata.name 2>/dev/null \
        | grep "^${release_name}-stack-data-ums-" | tail -1 || true)

    if [[ -n "$sdu_job" ]]; then
        local sdu_complete
        sdu_complete=$(kubectl get job "${sdu_job}" -n "${ns}" \
            -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' \
            2>/dev/null || echo "")
        if [[ "${sdu_complete}" == "True" ]]; then
            success "stack-data-ums job '${sdu_job}' is already Complete."
            info "register-consumers may still be catching up — wait a moment and re-run."
            return 0
        fi
        warn "Removing existing non-complete stack-data-ums job '${sdu_job}'..."
        if [[ "${DRY_RUN}" != "1" ]]; then
            kubectl delete job "${sdu_job}" -n "${ns}" \
                --ignore-not-found=true >/dev/null 2>&1 || true
        else
            info "  [dry-run] Would: kubectl delete job ${sdu_job} -n ${ns}"
        fi
    fi

    # ── 3. Extract and apply the job from the Helm release ───────────────────
    # NOTE: the stack-data-ums job is a Helm hook so it only appears in
    # `helm get all`, NOT in `helm get manifest`.
    info "Applying stack-data-ums job from Helm release into namespace '${ns}'..."
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "  [dry-run] Would: helm get all ${release_name} -n ${ns} | python3 ... | kubectl apply -n ${ns} -f -"
        return 0
    fi

    helm get all "${release_name}" -n "${ns}" 2>/dev/null \
        | python3 -c "
import sys
for section in sys.stdin.read().split('---'):
    if 'stack-data-ums' in section and 'kind: \"Job\"' in section:
        print('---')
        print(section.strip())
        break
" \
        | kubectl apply -n "${ns}" -f - >/dev/null || {
        warn "Failed to apply stack-data-ums job — is the nubus Helm release deployed?"
        warn "  helm list -n ${ns}"
        return 1
    }

    # ── 4. Wait for the job to appear and complete ───────────────────────────
    info "Waiting for stack-data-ums job to appear in namespace '${ns}' (up to 2m)..."
    local deadline=$((SECONDS + 120))
    local new_job=""
    until [[ -n "$new_job" ]]; do
        new_job=$(kubectl get jobs -n "${ns}" --no-headers \
            -o custom-columns=NAME:.metadata.name 2>/dev/null \
            | grep "^${release_name}-stack-data-ums-" | tail -1 || true)
        if (( SECONDS > deadline )); then
            warn "stack-data-ums job did not appear within 2m."
            warn "  Check: kubectl get jobs -n ${ns}"
            return 1
        fi
        [[ -n "$new_job" ]] || sleep 3
    done

    info "Waiting for stack-data-ums job '${new_job}' to complete (up to 10m)..."
    if kubectl wait "job/${new_job}" -n "${ns}" \
            --for=condition=Complete --timeout=600s 2>/dev/null; then
        success "stack-data-ums job completed — portal stack should recover within a few minutes."
        info "  Monitor: kubectl get pods -n ${ns} -l app.kubernetes.io/component=portal-consumer"
    else
        warn "stack-data-ums job did not complete within 10m."
        warn "  Check: kubectl logs -n ${ns} -l job-name=${new_job} --tail=40"
        return 1
    fi
}

# =============================================================================
# main
# =============================================================================
echo ""
echo -e "\033[0;36m╔══════════════════════════════════════════════════╗\033[0m"
echo -e "\033[0;36m║     Gentian OS — Day-2 Update                    ║\033[0m"
if [[ "${DRY_RUN}" == "1" ]]; then
echo -e "\033[0;36m║     (dry-run — no changes will be applied)       ║\033[0m"
fi
echo -e "\033[0;36m╚══════════════════════════════════════════════════╝\033[0m"
echo ""

_init

[[ "${OP_MAIL}"            == "1" ]] && op_mail
[[ "${OP_SECRETS}"         == "1" ]] && op_secrets
[[ "${OP_CROSSPLANE}"      == "1" ]] && op_crossplane_update
[[ "${OP_APPPROFILES}"     == "1" ]] && op_appprofiles_bootstrap
[[ "${OP_RECONCILE}"       == "1" ]] && op_reconcile_releases
[[ "${OP_LDAP_ACL}"        == "1" ]] && op_ldap_acl_upgrade
[[ "${OP_NUBUS_RECOVER}"   == "1" ]] && op_nubus_recover
[[ "${OP_KEYCLOAK_SYNC}"        == "1" ]] && op_keycloak_sync
[[ "${OP_FIX_KERNEL_LDAP_SCOPE}" == "1" ]] && op_fix_kernel_ldap_scope
[[ "${OP_PLUGIN}"               == "1" ]] && install_app_catalogue
[[ "${OP_ACME_ISSUERS}"         == "1" ]] && op_acme_issuers

echo ""
success "update.sh completed."

