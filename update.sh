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
#   ./update.sh --all                      # Run all Suze-path update operations
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
# KERNEL_NAMESPACE is set in _init from SERVICES_NAMESPACE / ENV after loading install.env.

# ─── Defaults ─────────────────────────────────────────────────────────────────
DRY_RUN=0
OP_MAIL=0
OP_SECRETS=0
OP_RECONCILE=0
OP_CROSSPLANE=0
OP_APPPROFILES=0
OP_ARGOCD=0
OP_PORTAL=0
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
  --crossplane             Re-apply Crossplane XRDs and Compositions from the
                           repository (tenant-default manifest bridge, app-*).
                           Run after Crossplane XRD/Composition changes; included in --all.
  --appprofiles            Ensure the gentian-catalogue ApplicationSet
                           exists so AppProfile CRs are kept in sync from the
                           gentian-apps repository.
  --argocd                 Re-apply gentian-os / appprofiles ArgoCD Application
                           manifests (ignoreDifferences updates) and hard-refresh
                           all Applications.
  --portal                 Reconcile Gentian portal login: Keycloak clients
                           (gentian-portal + BFF), platform admin, Helm upgrade
                           of gentian-portal-web/api (same as install.sh
                           --stage1-portal).
  --plugin                 Reinstall the kubectl-gentian plugin from this
                           repository (idempotent: skips if already up-to-date).
  --acme-issuers           Re-apply cert-manager ClusterIssuers from install.env
                           (ACME_ENV=production or staging). Not included in --all.
  --all                    Run Suze-path update operations (default when no options).
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
        --crossplane)          OP_CROSSPLANE=1 ;;
        --appprofiles)         OP_APPPROFILES=1 ;;
        --argocd)              OP_ARGOCD=1 ;;
        --portal)              OP_PORTAL=1 ;;
        --plugin)              OP_PLUGIN=1 ;;
        --acme-issuers)        OP_ACME_ISSUERS=1 ;;
        --all)                 OP_MAIL=1; OP_SECRETS=1; OP_RECONCILE=1; OP_CROSSPLANE=1; OP_APPPROFILES=1; OP_ARGOCD=1; OP_PLUGIN=1 ;;
        --dry-run)             DRY_RUN=1 ;;
        -h|--help)             _usage ;;
        *) echo "Unknown option: $1" >&2; _usage ;;
    esac
    shift
done

# Default: reconcile everything when no specific operation is requested.
if [[ "${OP_MAIL}" == "0" && "${OP_SECRETS}" == "0" && "${OP_RECONCILE}" == "0" && "${OP_CROSSPLANE}" == "0" && "${OP_APPPROFILES}" == "0" && "${OP_ARGOCD}" == "0" && "${OP_PORTAL}" == "0" && "${OP_PLUGIN}" == "0" && "${OP_ACME_ISSUERS}" == "0" ]]; then
    OP_MAIL=1
    OP_SECRETS=1
    OP_RECONCILE=1
    OP_CROSSPLANE=1
    OP_APPPROFILES=1
    OP_ARGOCD=1
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
    load_deployments_cluster_settings

    KERNEL_NAMESPACE="${SERVICES_NAMESPACE:-gentian-${ENV:-dev}}"
    export KERNEL_NAMESPACE

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

# _detect_deployed_mail_mode, _postfix_dev_values_yaml, _patch_postfix_configmap,
# install_stage1_mail — see scripts/mail-lib.sh (sourced via install-lib.sh).

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
            warn "MAIL_SERVICE_MODE drift (${deployed} → ${mode}) — MAIL_SERVICE_MODE is fixed at install time."
            warn "  Patching postfix-dev-values for recovery; prefer a clean reinstall when switching mail modes."
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
            --arg user "${SMTP_RELAY_USERNAME:-}" \
            --arg pass "${SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"

    # ── 3. Seed mail/dovecot in OpenBao (required for Dovecot ESO to sync) ────
    info "Seeding mail/dovecot credentials in crossplane-system..."
    _kv_secret "gentian-os-kernel-mail-dovecot" \
        "$(jq -nc \
            --arg doveadm "$(_derive dovecot doveadm_password)" \
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
        if ! mail_network_mode_compatible "${mode}" "${NETWORK_MODE:-tunnel}"; then
            error "$(mail_network_mode_incompatibility_message)"
            exit 1
        fi
        deploy_kernel_mail_services
        _sync_kernel_postfix_virtual_mailbox_maps
        _patch_postfix_configmap kernel
        _patch_postfix_allowed_sender_domains
        _patch_postfix_live_relay kernel
        verify_dovecot_installation || warn "Dovecot verification failed after mail reconciliation."
    elif kubectl get configmap postfix-dev-values -n "${KERNEL_NAMESPACE:-gentian-${ENV:-dev}}" >/dev/null 2>&1; then
        _patch_postfix_configmap external
        _patch_postfix_allowed_sender_domains
        _patch_postfix_live_relay external
    fi

    # ── 6. Configure Keycloak realm SMTP (invite/reset password emails) ─────────
    if [[ "${DRY_RUN}" != "1" ]] \
        && kubectl get secret keycloak-admin -n platform-kernel >/dev/null 2>&1; then
        # shellcheck source=scripts/portal-login-bootstrap.sh
        source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
        configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."
        configure_tenant_realms_smtp || warn "Tenant realm SMTP configuration skipped."
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
            --arg user "${SMTP_RELAY_USERNAME:-}" \
            --arg pass "${SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"

    info "Re-seeding mail/dovecot..."
    _kv_secret "gentian-os-kernel-mail-dovecot" \
        "$(jq -nc \
            --arg doveadm "$(_derive dovecot doveadm_password)" \
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
                    _apply_kernel_manifest_dir "${manifest_dir}" all
                    any_action=1
                else
                    info "  [dry-run] Would: apply kernel manifests in ${manifest_dir}/"
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
            _apply_kernel_manifest_dir "${manifest_dir}" all

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
            _apply_kernel_manifest_dir "${manifest_dir}" release
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
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        op_staging_ca_secret
    fi
}

# =============================================================================
# op_staging_ca_secret — CA bundle for in-cluster TLS to id.<kernel-domain>
# =============================================================================
op_staging_ca_secret() {
    banner "gentian-staging-ca-tls (ACME staging CA trust bundle)"
    local script="${SCRIPT_DIR}/scripts/create-staging-ca-secret.sh"
    if [[ ! -x "$script" ]]; then
        chmod +x "$script"
    fi
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would run: $script ${SERVICES_NAMESPACE:-gentian-${ENV:-dev}}"
        return 0
    fi
    "$script" "${SERVICES_NAMESPACE:-gentian-${ENV:-dev}}"
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
# op_appprofiles_bootstrap — ensure the gentian-catalogue ApplicationSet exists so
# profile bundles (AppProfile + compositions + assets) sync from gentian-apps.
# =============================================================================
op_appprofiles_bootstrap() {
    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would apply gentian-catalogue ApplicationSet"
        return 0
    fi

    install_catalogue_sync

    kubectl annotate applicationset gentian-catalogue -n argocd \
        "argocd.argoproj.io/refresh=hard" --overwrite >/dev/null 2>&1 || true

    info "  Monitor: kubectl get applicationset gentian-catalogue -n argocd"
    info "  Bundles: kubectl get applications -n argocd | grep '^catalogue-'"
}

# =============================================================================
# op_argocd_bootstrap — re-apply operator ArgoCD Application manifests and
#                       refresh all Applications (picks up ignoreDifferences).
# =============================================================================
op_argocd_bootstrap() {
    banner "ArgoCD Application manifest reconciliation"

    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
    local gentian_os_branch
    gentian_os_branch=$(git -C "${SCRIPT_DIR}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "develop")
    local tmpl="${SCRIPT_DIR}/kernel/bootstrap/gentian-os-application.yaml.tmpl"

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would apply gentian-os Application + gentian-tenants ApplicationSet"
        info "[dry-run] would hard-refresh all ArgoCD Applications"
        return 0
    fi

    if [[ -f "${tmpl}" ]]; then
        info "Re-applying gentian-os Application + gentian-tenants ApplicationSet (branch=${gentian_os_branch})..."
        sed -e "s|%GENTIAN_OS_BRANCH%|${gentian_os_branch}|g" \
            -e "s|%DEPLOYMENTS_REPO%|${GENTIAN_DEPLOYMENTS_REPO:-https://github.com/gentian-org/gentian-deployments}|g" \
            -e "s|%DEPLOYMENTS_BRANCH%|${GENTIAN_DEPLOYMENTS_BRANCH:-main}|g" \
            -e "s|%CLUSTER%|${cluster}|g" \
            -e "s|%STAGE%|${stage}|g" \
            "${tmpl}" | kubectl apply -f -
        success "gentian-os Application manifest updated."
        release_gentian_os_helm_bootstrap "gentian-system"
    else
        warn "Template not found: ${tmpl}"
    fi

    op_appprofiles_bootstrap

    # Pick up ApplicationSet template changes (ignoreDifferences, etc.) without
    # waiting for gentian-appsets to poll git.

    info "Hard-refreshing all ArgoCD Applications..."
    kubectl get applications -n argocd -o name 2>/dev/null \
        | xargs -r -I{} kubectl annotate {} -n argocd \
            "argocd.argoproj.io/refresh=hard" --overwrite >/dev/null 2>&1 || true

    verify_argocd_apps || true
}

# =============================================================================
# op_portal — reconcile Gentian portal login (Keycloak + gentian-portal ArgoCD)
# =============================================================================
op_portal() {
    banner "Portal login reconciliation (Stage 1)"

    [[ -n "${KERNEL_DOMAIN:-}" ]] || {
        echo "ERROR: KERNEL_DOMAIN not set — check gentian-deployments cluster-settings.env." >&2
        exit 1
    }

    if [[ "${DRY_RUN}" == "1" ]]; then
        info "[dry-run] would run install_stage1_portal (Keycloak bootstrap + gentian-portal ArgoCD)"
        return 0
    fi

    # shellcheck source=scripts/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
    install_stage1_portal
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
[[ "${OP_ARGOCD}"          == "1" ]] && op_argocd_bootstrap
[[ "${OP_PORTAL}"          == "1" ]] && op_portal
[[ "${OP_RECONCILE}"       == "1" ]] && op_reconcile_releases
[[ "${OP_PLUGIN}"          == "1" ]] && install_app_catalogue
[[ "${OP_ACME_ISSUERS}"    == "1" ]] && op_acme_issuers

echo ""
success "update.sh completed."

