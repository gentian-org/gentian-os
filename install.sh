#!/usr/bin/env bash
# =============================================================================
# install.sh — Gentian OS bootstrap (Crossplane-based)
# =============================================================================
#
# After this script completes:
#   ✓ Crossplane core installed with provider-kubernetes, provider-vault,
#     function-go-templating
#   ✓ cert-manager, External Secrets Operator, ArgoCD running
#   ✓ OpenBao initialized and unsealed (transit auto-unseal)
#   ✓ Cluster XR has provisioned all kernel structural resources:
#       - OpenBao KV mount + policies + Kubernetes auth backend/roles
#       - KV seed paths for database, cache, storage, identity, mail
#       - ArgoCD AppProject (gentianos-tenants)
#       - ESO ClusterSecretStore (openbao)
#       - cert-manager ClusterIssuer (letsencrypt-http01)
#   ✓ XTenant XRD + tenant-default Composition:
#       - Operator seeds OpenBao credentials and writes tenant-*-provisioning-jobs
#       - Crossplane applies identity, data-plane, and edge resources declaratively
#   ✓ Remaining secrets seeded (registry, DNS/Cloudflare, internal)
#
# Upgrade note: this script targets greenfield installs. Legacy version-to-version
# migration paths (pre-InfraData layouts, flat gentian-appprofiles Application,
# etc.) were removed in cleanup batch b. Remaining delete/reset hooks heal failed
# or partial installs only (Suze ghost Helm releases, orphaned Crossplane RBAC).
#
# Usage:
#   ./install.sh
#   ./install.sh --validate          # validate config only, no cluster changes
#   ./install.sh --no-cluster-infra  # skip cert-manager/CNPG/reloader
#
# Required environment variables: same as install.sh (see getting-started.md)
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load all helper functions from scripts/lib/load.sh.
# shellcheck source=scripts/lib/load.sh
source "${SCRIPT_DIR}/scripts/lib/load.sh"

# ─── Crossplane settings ──────────────────────────────────────────────────────
CROSSPLANE_NAMESPACE=crossplane-system
CROSSPLANE_VERSION="2.2.1"
CROSSPLANE_HELM_REPO=https://charts.crossplane.io/stable
PROVIDER_WAIT_TIMEOUT=15m
CLUSTER_XR_TIMEOUT=15m

# ─── OpenBao CLI — auto-install if missing ────────────────────────────────────
# Matches the appVersion of kernel/bootstrap/openbao-application.yaml chart 0.25.5.
# Installed to ~/.local/bin so no sudo is required.
OPENBAO_CLI_VERSION="${OPENBAO_CLI_VERSION:-v2.5.0}"

_ensure_bao() {
    if command -v bao >/dev/null 2>&1; then
        return 0
    fi
    local _os _arch _archive _install_dir
    _os=$(uname -s | tr '[:upper:]' '[:lower:]')
    _arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
    _archive="bao_${OPENBAO_CLI_VERSION#v}_${_os}_${_arch}.zip"
    _install_dir="${HOME}/.local/bin"
    mkdir -p "${_install_dir}"
    info "bao CLI not found — installing ${OPENBAO_CLI_VERSION} to ${_install_dir}..."
    curl -fsSL \
        "https://github.com/openbao/openbao/releases/download/${OPENBAO_CLI_VERSION}/${_archive}" \
        -o "/tmp/${_archive}"
    unzip -qo "/tmp/${_archive}" bao -d "${_install_dir}"
    rm -f "/tmp/${_archive}"
    chmod +x "${_install_dir}/bao"
    export PATH="${_install_dir}:${PATH}"
    success "bao ${OPENBAO_CLI_VERSION} installed to ${_install_dir}/bao"
}

_ensure_crossplane_package_crds() {
    local required missing=()
    required=(
        providers.pkg.crossplane.io
        providerrevisions.pkg.crossplane.io
        functions.pkg.crossplane.io
        functionrevisions.pkg.crossplane.io
        deploymentruntimeconfigs.pkg.crossplane.io
    )

    for crd in "${required[@]}"; do
        if ! kubectl get crd "${crd}" >/dev/null 2>&1; then
            missing+=("${crd}")
        fi
    done

    if [[ "${#missing[@]}" -eq 0 ]]; then
        return 0
    fi

    warn "Crossplane package CRDs missing: ${missing[*]}"
    info "Re-applying Crossplane CRDs from Helm chart..."
    helm repo add crossplane-stable "${CROSSPLANE_HELM_REPO}" --force-update >/dev/null
    helm repo update >/dev/null
    # --server-side, not plain apply: some of these CRDs' embedded OpenAPI
    # schemas exceed the 256 KiB single-annotation limit once client-side
    # apply embeds the full manifest into kubectl.kubernetes.io/last-applied-
    # configuration (deploymentruntimeconfigs.pkg.crossplane.io hits this on
    # Crossplane 2.x). Server-side apply uses field-manager tracking instead
    # and has no such limit. --force-conflicts is safe here: these are
    # Crossplane's own canonical chart-managed objects, never hand-edited.
    helm template crossplane crossplane-stable/crossplane \
        --version "${CROSSPLANE_VERSION}" \
        --namespace "${CROSSPLANE_NAMESPACE}" \
        --include-crds \
        | kubectl apply --server-side --force-conflicts -f - >/dev/null

    # Some chart packaging modes do not include CRDs in Helm output. Ensure the
    # required package CRDs are explicitly applied from upstream release assets.
    local crossplane_minor="${CROSSPLANE_VERSION%.*}"
    for crd in providers providerrevisions functions functionrevisions deploymentruntimeconfigs; do
        kubectl apply --server-side --force-conflicts \
            -f "https://raw.githubusercontent.com/crossplane/crossplane/release-${crossplane_minor}/cluster/crds/pkg.crossplane.io_${crd}.yaml" \
            >/dev/null 2>&1 || true
    done

    for crd in "${required[@]}"; do
        kubectl wait --for=condition=Established "crd/${crd}" --timeout=90s >/dev/null 2>&1 || true
    done

    local unresolved=()
    for crd in "${required[@]}"; do
        if ! kubectl get crd "${crd}" >/dev/null 2>&1; then
            unresolved+=("${crd}")
        fi
    done
    if [[ "${#unresolved[@]}" -gt 0 ]]; then
        error "Crossplane CRDs still missing after re-apply: ${unresolved[*]}"
        exit 1
    fi
    success "Crossplane package CRDs are present."
}

# =============================================================================
# Crossplane 0 — Install Crossplane core
# (mirrors the logic of crossplane/tests/e2e/scripts/p0-crossplane-install.sh)
# =============================================================================
install_crossplane() {
    banner "Step 0 — Install Crossplane core"

    if kubectl get deployment crossplane -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1; then
        success "Crossplane deployment already present in ${CROSSPLANE_NAMESPACE}; skipping."
        _ensure_crossplane_package_crds
        return
    fi
    if helm status crossplane -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1; then
        success "Crossplane already installed via Helm; skipping."
        _ensure_crossplane_package_crds
        return
    fi

    # Remove orphaned cluster-scoped resources left by a prior failed install.
    if kubectl get clusterrole crossplane >/dev/null 2>&1; then
        info "Removing orphaned Crossplane cluster-scoped RBAC before install..."
        kubectl delete clusterrole \
            crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
            --ignore-not-found=true
        kubectl delete clusterrolebinding \
            crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
            --ignore-not-found=true
        kubectl delete namespace "${CROSSPLANE_NAMESPACE}" --ignore-not-found=true
    fi

    helm repo add crossplane-stable "${CROSSPLANE_HELM_REPO}" --force-update
    helm repo update
    helm install crossplane crossplane-stable/crossplane \
        --namespace "${CROSSPLANE_NAMESPACE}" \
        --create-namespace \
        --version "${CROSSPLANE_VERSION}" \
        --set replicas=1 \
        --wait --timeout 5m

    kubectl wait deployment/crossplane \
        -n "${CROSSPLANE_NAMESPACE}" \
        --for=condition=Available --timeout=5m
    _ensure_crossplane_package_crds
    success "Crossplane core installed and Ready."
}

# =============================================================================
# Crossplane 0b/0c — Install providers, XRD, Composition
# (mirrors crossplane/tests/e2e/scripts/p1-kernel-dev.sh steps 1-3)
# =============================================================================
install_crossplane_providers() {
    banner "Step 0b — Crossplane providers, XRD, Composition"

    info "Applying providers (function-go-templating, provider-kubernetes, provider-vault)..."
    _kubectl_retry apply -f "${SCRIPT_DIR}/crossplane/providers/providers.yaml"

    info "Waiting for providers to become Healthy (timeout: ${PROVIDER_WAIT_TIMEOUT})..."

    # function-go-templating and function-auto-ready are Function resources;
    # the rest are Provider resources. Use the correct type for each so
    # we don't burn the full timeout on the wrong resource kind.
    for fn in function-go-templating function-extra-resources function-auto-ready; do
        info "  Waiting for: ${fn}"
        _kubectl_retry wait "function.pkg.crossplane.io/${fn}" \
            --for=condition=Healthy --timeout="${PROVIDER_WAIT_TIMEOUT}"
    done

    for provider in provider-helm provider-kubernetes provider-vault; do
        info "  Waiting for: ${provider}"
        _kubectl_retry wait "provider.pkg.crossplane.io/${provider}" \
            --for=condition=Healthy --timeout="${PROVIDER_WAIT_TIMEOUT}"
    done

    # Apply ProviderConfigs only after all providers are Healthy so the CRDs
    # (e.g. vault.upbound.io/v1beta1 ProviderConfig) exist.
    info "Applying ProviderConfigs (InjectedIdentity for both kubernetes and openbao)..."
    _kubectl_retry apply -f "${SCRIPT_DIR}/crossplane/providers/provider-configs.yaml"

    # After a partial uninstall or a failed prior run the CRDs that Crossplane
    # creates for each XRD (e.g. xapps.gentianos.io, apps.gentianos.io) can
    # survive with ownerReferences pointing to the now-deleted XRD object UID.
    # The XRD controller refuses to adopt CRDs owned by a different UID and
    # the XRD never reaches Established.  Fix: after applying an XRD, if any
    # of its owned CRDs still carry a stale UID, patch them to the current one.
    _adopt_xrd_crds() {
        local xrd_name="$1"; shift   # e.g. xapps.gentianos.io
        local -a crds=("$@")        # e.g. xapps.gentianos.io apps.gentianos.io
        local xrd_uid
        xrd_uid=$(kubectl get compositeresourcedefinition "${xrd_name}" \
            -o jsonpath='{.metadata.uid}' 2>/dev/null) || return 0
        for crd in "${crds[@]}"; do
            local owner_uid
            owner_uid=$(kubectl get crd "${crd}" \
                -o jsonpath='{.metadata.ownerReferences[0].uid}' 2>/dev/null) || continue
            if [[ -n "${owner_uid}" && "${owner_uid}" != "${xrd_uid}" ]]; then
                warn "  CRD ${crd} has stale ownerRef UID ${owner_uid} (XRD is ${xrd_uid}); patching..."
                kubectl patch crd "${crd}" --type=json \
                    -p="[{\"op\":\"replace\",\"path\":\"/metadata/ownerReferences/0/uid\",\"value\":\"${xrd_uid}\"}]" \
                    2>/dev/null || true
                success "  Patched ownerRef on ${crd}."
            fi
        done
    }

    info "Applying XRD (XCluster / Cluster)..."
    _kubectl_retry apply -f "${SCRIPT_DIR}/crossplane/xrds/cluster.yaml"
    _adopt_xrd_crds xclusters.gentianos.io xclusters.gentianos.io clusters.gentianos.io
    _kubectl_retry wait xrd xclusters.gentianos.io \
        --for=condition=Established --timeout=2m

    info "Applying XRD (XApp / App)..."
    _kubectl_retry apply -f "${SCRIPT_DIR}/crossplane/xrds/app.yaml"
    _adopt_xrd_crds xapps.gentianos.io xapps.gentianos.io apps.gentianos.io
    _kubectl_retry wait xrd xapps.gentianos.io \
        --for=condition=Established --timeout=2m

    apply_crossplane_platform_compositions

    info "Applying XRD (XTenant / Tenant)..."
    _kubectl_retry apply -f "${SCRIPT_DIR}/crossplane/xrds/tenant.yaml"
    _adopt_xrd_crds xtenants.gentianos.io xtenants.gentianos.io tenants.gentianos.io
    _kubectl_retry wait xrd xtenants.gentianos.io \
        --for=condition=Established --timeout=2m

    success "Crossplane providers, XRDs, and Compositions are ready."
}

# =============================================================================
# Crossplane step 10 — Bootstrap OpenBao's Kubernetes auth backend for
# provider-vault InjectedIdentity.
#
# provider-vault authenticates to OpenBao using the crossplane-provider-vault
# ServiceAccount token via the Kubernetes auth backend. On a fresh cluster,
# that backend doesn't exist yet — this step creates it with the root token
# so that the Cluster XR can reconcile on the first apply.
#
    # The `crossplane-write` policy in the Cluster XR Policy MR keeps the policy in
    # sync going forward. The Cluster XR uses Mount (KV engine), Policy, Backend,
    # AuthBackendConfig, AuthBackendRole, SecretV2, and Object (K8s) resources.
# =============================================================================
bootstrap_openbao_for_crossplane() {
    banner "Step 8 — Bootstrap OpenBao auth for Crossplane"

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    export VAULT_ADDR="https://${BAO_SVC_IP}:8200"
    export VAULT_SKIP_VERIFY=true

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "${OPENBAO_INIT_FILE}")
        else
            read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export VAULT_TOKEN="${BAO_TOKEN}"

    # ── 1. KV v2 mount — use KV_MOUNT from install.env (default: secret) ─────
    # Must match spec.openbao.kvMount in dev-cluster.yaml.tmpl and the
    # cluster-default Composition, which also uses this env var.
    local _kv_mount="${KV_MOUNT:-secret}"
    if bao secrets list -format=json 2>/dev/null | jq -e --arg m "${_kv_mount}/" '.[($m)]' >/dev/null 2>&1; then
        success "KV v2 mount at '${_kv_mount}/' already present."
    else
        bao secrets enable -path="${_kv_mount}" kv-v2
        success "KV v2 mount at '${_kv_mount}/' enabled."
    fi

    # ── 2. Kubernetes auth backend ────────────────────────────────────────────
    if bao auth list -format=json 2>/dev/null | jq -e '."kubernetes/"' >/dev/null 2>&1; then
        success "Kubernetes auth backend already present."
    else
        bao auth enable -path=kubernetes kubernetes
        success "Kubernetes auth backend enabled."
    fi
    bao write auth/kubernetes/config \
        kubernetes_host="https://kubernetes.default.svc"
    success "Kubernetes auth backend configured."

    # ── 3. crossplane-write policy (broad — provider-vault needs sys/* access) ─
    # The Cluster XR Policy MR will keep this policy in sync going forward.
    # _kv_mount is substituted into the heredoc via a quoted-less delimiter so
    # the shell expands the variable before passing the policy to bao.
    bao policy write crossplane-write - <<POLICY
# KV operations
path "${_kv_mount}/data/gentian-os/*"     { capabilities = ["create","read","update","delete"] }
path "${_kv_mount}/metadata/gentian-os/*" { capabilities = ["list","read","delete"] }
# Mount management (SecretMount MR)
path "sys/mounts/*"   { capabilities = ["create","read","update","delete","sudo"] }
path "sys/mounts"     { capabilities = ["read","list"] }
# Policy management (Policy MRs)
path "sys/policies/acl/*" { capabilities = ["create","read","update","delete","list"] }
path "sys/policies/acl"   { capabilities = ["read","list"] }
# Auth method management (Backend/BackendConfig/BackendRole MRs)
path "sys/auth/*"  { capabilities = ["create","read","update","delete","sudo"] }
path "sys/auth"    { capabilities = ["read"] }
path "auth/+/config"  { capabilities = ["create","read","update"] }
path "auth/+/role/*"  { capabilities = ["create","read","update","delete","list"] }
# Token operations
path "auth/token/create"      { capabilities = ["update"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
POLICY
    success "crossplane-write policy written."

    # ── 3b. eso-read policy ───────────────────────────────────────────────────
    # ESO reads all kernel + tenant app secrets. Tenant isolation is enforced
    # by Kubernetes RBAC on the resulting Secrets; ESO needs one cluster-wide role.
    bao policy write eso-read - <<POLICY
path "${_kv_mount}/data/gentian-os/kernel/*"    { capabilities = ["read"] }
path "${_kv_mount}/metadata/gentian-os/kernel/*" { capabilities = ["list"] }
path "${_kv_mount}/data/gentian-os/tenants/+/apps/*"  { capabilities = ["read"] }
path "${_kv_mount}/metadata/gentian-os/tenants/*"       { capabilities = ["list"] }
POLICY
    success "eso-read policy written."

    # ── 4. Kubernetes auth roles ──────────────────────────────────────────────
    # crossplane-provider: kept for future dynamic-token use (not used by
    # provider-vault ProviderConfig which reads a static token Secret).
    bao write auth/kubernetes/role/crossplane-provider \
        bound_service_account_names=crossplane-provider-vault \
        bound_service_account_namespaces="${CROSSPLANE_NAMESPACE}" \
        token_policies=crossplane-write \
        token_ttl=3600
    success "crossplane-provider K8s auth role created."

    # eso: External Secrets Operator reads all kernel and tenant app secrets.
    bao write auth/kubernetes/role/eso \
        bound_service_account_names=external-secrets \
        bound_service_account_namespaces=external-secrets \
        token_policies=eso-read \
        token_ttl=3600
    success "eso K8s auth role created."

    # ── 5. Mint periodic crossplane token + store as k8s Secret ──────────────
    # provider-vault v3.x (upjet/Terraform-based) does not support
    # InjectedIdentity. It reads credentials from a k8s Secret whose 'credentials'
    # key must contain a JSON object with a 'token' field.
    # Validate existing Secret before re-minting to stay idempotent.
    local need_new_token=1
    if kubectl get secret openbao-crossplane-token -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1; then
        local existing_token
        existing_token=$(kubectl get secret openbao-crossplane-token -n "${CROSSPLANE_NAMESPACE}" \
            -o jsonpath='{.data.credentials}' 2>/dev/null \
            | base64 -d 2>/dev/null \
            | jq -r '.token // empty' 2>/dev/null || true)
        if [[ -n "${existing_token}" ]]; then
            local http_code
            http_code=$(curl -k -s -o /dev/null -w '%{http_code}' --max-time 5 \
                -H "X-Vault-Token: ${existing_token}" \
                "${VAULT_ADDR}/v1/auth/token/lookup-self" 2>/dev/null || echo 000)
            if [[ "${http_code}" == "200" ]]; then
                success "openbao-crossplane-token Secret already valid — skipping."
                need_new_token=0
            else
                info "Existing openbao-crossplane-token is stale (HTTP ${http_code}); recreating."
                kubectl delete secret openbao-crossplane-token \
                    -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1 || true
            fi
        fi
    fi

    if [[ "${need_new_token}" == "1" ]]; then
        info "Minting periodic crossplane-provider token (period=8760h)..."
        local cp_token
        cp_token=$(bao token create \
            -policy=crossplane-write \
            -period=8760h \
            -orphan \
            -display-name=crossplane-provider \
            -format=json \
            | jq -r '.auth.client_token')
        if [[ -z "${cp_token}" || "${cp_token}" == "null" ]]; then
            error "Failed to mint crossplane-provider token."
            exit 1
        fi
        kubectl create secret generic openbao-crossplane-token \
            -n "${CROSSPLANE_NAMESPACE}" \
            --from-literal=credentials="{\"token\":\"${cp_token}\"}"
        success "openbao-crossplane-token Secret created in ${CROSSPLANE_NAMESPACE}."
    fi

    info "provider-vault ProviderConfig will authenticate via openbao-crossplane-token Secret."

    # Scrub the root token from the process environment so it does not remain
    # visible in /proc/<pid>/environ or child-process env for the rest of
    # the install run.  VAULT_ADDR is kept (harmless; it is a plain URL).
    unset VAULT_TOKEN
}

# =============================================================================
# Crossplane step 11 — Create derived-credential Secrets in crossplane-system.
#
# The Cluster XR's SecretV2 KV-seed MRs reference these K8s Secrets via
# dataJsonSecretRef. Uses the same HMAC-SHA256 derivation as seed-openbao.sh.
# --dry-run=client | kubectl apply ensures idempotency.
# =============================================================================
create_crossplane_secrets() {
    banner "Step 9 — Create derived-credential Secrets for Cluster XR"

    # Enforce minimum-entropy on MASTER_PASSWORD
    if [[ ${#MASTER_PASSWORD} -lt 16 ]]; then
        error "MASTER_PASSWORD is too weak. It must be at least 16 characters long."
        exit 1
    fi

    # Try to read existing master-password and salt from OpenBao
    local existing_secret
    existing_secret=$(bao kv get -mount=secret -format=json gentian-os/kernel/internal/master-password 2>/dev/null || true)
    if [[ -n "${existing_secret}" ]]; then
        local m_val s_val
        m_val=$(echo "${existing_secret}" | jq -r '.data.data.value // empty' 2>/dev/null || true)
        s_val=$(echo "${existing_secret}" | jq -r '.data.data.salt // empty' 2>/dev/null || true)
        if [[ -n "${m_val}" ]]; then
            MASTER_PASSWORD="${m_val}"
        fi
        if [[ -n "${s_val}" ]]; then
            DERIVATION_SALT="${s_val}"
        elif [[ -n "${m_val}" ]]; then
            DERIVATION_SALT=""
        fi
    fi
    if [[ -z "${DERIVATION_SALT:-}" && -z "${existing_secret}" ]]; then
        DERIVATION_SALT=$(openssl rand -hex 16)
    fi
    export DERIVATION_SALT

    # Same derivation as seed-openbao.sh and crossplane/functions/derive-secrets/derive.py
    _derive() {
        if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
            openssl rand -hex 32
        else
            echo -n "${1}:${2}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT}" | awk '{print $2}';
        fi
    }
    _nats()   { echo "n$(_derive "$1" "$2")"; }

    # Helper: upsert a K8s Secret in crossplane-system with data.json key
    _kv_secret() {
        local name="$1" json="$2"
        kubectl create secret generic "${name}" \
            -n "${CROSSPLANE_NAMESPACE}" \
            "--from-literal=data.json=${json}" \
            --dry-run=client -o yaml | kubectl apply -f -
        success "  ${name}"
    }

    # master-password Secret (referenced by spec.masterPasswordSecretRef in the Cluster claim)
    kubectl create secret generic gentian-os-master-password \
        -n "${CROSSPLANE_NAMESPACE}" \
        --from-literal=password="${MASTER_PASSWORD}" \
        --from-literal=salt="${DERIVATION_SALT}" \
        --dry-run=client -o yaml | kubectl apply -f -
    success "  gentian-os-master-password"

    # ── database/postgresql ───────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-database-postgresql" \
        "$(jq -nc \
            --arg a "$(_derive postgres postgres_user)" \
            --arg b "$(_derive postgres keycloak_user)" \
            --arg c "$(_derive postgres keycloak_extensions_user)" \
            --arg h "$(_derive postgres openfga_user)" \
            '{postgres_password:$a,keycloak_user_password:$b,keycloak_extensions_user_password:$c,openfga_user_password:$h}')"

    # ── database/mariadb ──────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-database-mariadb" \
        "$(jq -nc \
            --arg a "$(_derive mariadb root_password)" \
            '{root_password:$a}')"

    # ── cache/redis ───────────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-cache-redis" \
        "$(jq -nc \
            --arg a "$(_derive redis password)" \
            '{auth_password:$a}')"

    # ── storage/minio ─────────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-storage-minio" \
        "$(jq -nc \
            --arg a "minio" \
            --arg b "$(_derive minio root_password)" \
            '{root_user:$a,root_password:$b}')"

    # ── identity/keycloak-bootstrap (Suze Keycloak admin password) ─────────────
    _kv_secret "gentian-os-kernel-identity-keycloak-bootstrap" \
        "$(jq -nc \
            --arg a "$(_derive keycloak adminPassword)" \
            '{admin_password:$a}')"

    # ── authz/openfga ─────────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-authz-openfga" \
        "$(jq -nc \
            --arg a "$(_derive openfga preshared_key)" \
            '{preshared_key:$a}')"

    # ── mail/postfix (HMAC-derived fields + operator-supplied relay credentials) ─
    _kv_secret "gentian-os-kernel-mail-postfix" \
        "$(jq -nc \
            --arg host "${EXTERNAL_SMTP_HOST:-}" \
            --arg port "${EXTERNAL_SMTP_PORT:-587}" \
            --arg user "${SMTP_RELAY_USERNAME:-}" \
            --arg pass "${SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"

    # ── mail/dovecot (HMAC-derived; only active when MAIL_SERVICE_MODE=kernel) ─
    # The Cluster XR creates a SecretV2 MR for this path and will seed OpenBao
    # on first apply. The doveadm_password shares its derivation namespace with
    # the minio secret for cross-service derivation consistency.
    _kv_secret "gentian-os-kernel-mail-dovecot" \
        "$(jq -nc \
            --arg doveadm "$(_derive dovecot doveadm_password)" \
            --arg oidc "$(_derive dovecot oidcClientSecret)" \
            '{doveadm_password:$doveadm,oidc_client_secret:$oidc}')"

    success "All 9 input Secrets applied to ${CROSSPLANE_NAMESPACE}."
}

# =============================================================================
# scaffold_cluster_deployment — Day-0 only. If this cluster's kernel/
# directory in gentian-deployments is missing claims/{cluster,infra-data,
# suze}.yaml or values.yaml, generate them from KERNEL_DOMAIN/
# GENTIAN_DEPLOYMENTS_STAGE and commit + push directly to main (no PR —
# this is scaffolding a not-yet-running cluster, not changing a live one;
# see docs/deployment.md §3). Per-file checks, not a directory-level one:
# never overwrites a file that already exists, so this is a no-op on every
# subsequent run, and it converges correctly even when cluster-settings.env
# already exists but the mechanical files don't.
#
# The gentian-os/gentian-portal Applications and the ImageUpdater CR are
# NOT scaffolded here — they're rendered directly from
# kernel/bootstrap/{gentian-os,gentian-portal}-application.yaml.tmpl by
# install_gentian_os_operator()/install_portal_login() (catalogue.sh /
# portal-login-bootstrap.sh) and applied straight to the cluster, never
# committed to gentian-deployments. Their content never varies except by
# %CLUSTER%/%STAGE%, so there's nothing cluster-specific worth persisting
# as a file — see docs/deployment.md §3.1.
# =============================================================================
scaffold_cluster_deployment() {
    local kernel_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/kernel"
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER}"
    local domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN must be resolved before scaffold_cluster_deployment}"
    local generated=0

    if [[ ! -f "${GENTIAN_DEPLOYMENTS_PATH}/profiles/${stage}.yaml" ]]; then
        warn "gentian-deployments/profiles/${stage}.yaml does not exist yet."
        warn "  Stage-tier policy (logLevel, ACME issuer, etc.) has no home for '${stage}' —"
        warn "  add it (see profiles/dev.yaml for the existing example) before continuing."
    fi
    if [[ ! -f "${GENTIAN_DEPLOYMENTS_PATH}/profiles/_base.yaml" ]]; then
        warn "gentian-deployments/profiles/_base.yaml does not exist yet."
        warn "  Cross-stage shared policy (platformSecurityPolicy, etc.) has no home —"
        warn "  add it before continuing (see profiles/_base.yaml in an existing cluster's repo)."
    fi

    mkdir -p "${kernel_dir}/claims"

    if [[ ! -f "${kernel_dir}/claims/cluster.yaml" ]]; then
        cat > "${kernel_dir}/claims/cluster.yaml" <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: dev-cluster
  namespace: crossplane-system
spec:
  kernelDomain: ${domain}
EOF
        info "Scaffolded ${kernel_dir}/claims/cluster.yaml"
        generated=1
    fi

    if [[ ! -f "${kernel_dir}/claims/infra-data.yaml" ]]; then
        cat > "${kernel_dir}/claims/infra-data.yaml" <<EOF
apiVersion: gentianos.io/v1alpha1
kind: InfraData
metadata:
  name: dev-infra-data
  namespace: crossplane-system
spec:
  environment: ${stage}
  compositeDeletePolicy: Background
EOF
        info "Scaffolded ${kernel_dir}/claims/infra-data.yaml"
        generated=1
    fi

    if [[ ! -f "${kernel_dir}/claims/suze.yaml" ]]; then
        cat > "${kernel_dir}/claims/suze.yaml" <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Suze
metadata:
  name: dev-suze
  namespace: crossplane-system
spec:
  environment: ${stage}
  idpNamespace: platform-kernel
  compositeDeletePolicy: Background
  openfga:
    chartVersion: "0.3.10"
EOF
        info "Scaffolded ${kernel_dir}/claims/suze.yaml"
        generated=1
    fi

    if [[ ! -f "${kernel_dir}/values.yaml" ]]; then
        cat > "${kernel_dir}/values.yaml" <<EOF
# Cluster overlay — only what's unique to THIS cluster. Tier-wide policy
# lives in gentian-deployments/profiles/${stage}.yaml (Layer 2); chart
# defaults live in gentian-os/charts/gentian-os/values.yaml (Layer 1).
# Also read directly by the gentian-portal Application for kernelDomain
# (portal chart is separate from the operator chart but shares this file).
kernelDomain: ${domain}
stage: ${stage}

image:
  tag: "develop"

# Uncomment and fill in for Cloudflare edge-DNS/tunnel mode:
# cloudflare:
#   zoneID: ""
#   tunnelCNAME: ""

api:
  env:
    BACKEND_CORS_ORIGINS: https://portal.${domain}
EOF
        info "Scaffolded ${kernel_dir}/values.yaml"
        generated=1
    fi

    if (( generated )); then
        (
            cd "${GENTIAN_DEPLOYMENTS_PATH}"
            git add "clusters/${cluster}/kernel"
            git commit -m "Scaffold clusters/${cluster}/kernel (stage=${stage}, kernelDomain=${domain})"
            git push origin "$(git rev-parse --abbrev-ref HEAD)"
        )
        success "Scaffolded and pushed clusters/${cluster}/kernel to gentian-deployments."
    else
        info "clusters/${cluster}/kernel already fully scaffolded — nothing to do."
    fi
}

# =============================================================================
# Crossplane step 12 — Apply Cluster claim and wait for Ready.
# The Cluster XR creates all 19 kernel MRs via provider-vault and
# provider-kubernetes. managementPolicies: [Observe,Create] on KV seeds
# ensures existing paths seeded by prior install runs are never overwritten.
# =============================================================================
apply_cluster_xr() {
    banner "Step 10 — Apply Cluster XR (kernel structural provisioning)"

    local claims_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/kernel/claims"
    [[ -f "${claims_dir}/cluster.yaml" ]] || {
        error "No Cluster claim at ${claims_dir}/cluster.yaml — run install.sh's cluster scaffolding step first."
        exit 1
    }

    info "Applying Cluster claim from ${claims_dir}/cluster.yaml..."
    kubectl apply -f "${claims_dir}/cluster.yaml"

    # Crossplane generates a unique name for the XCluster composite (e.g.
    # dev-cluster-k4d2m). Read it from the Claim's resourceRef once populated.
    info "Waiting for Claim dev-cluster to be bound to a composite (up to 60s)..."
    local xr_name=""
    local deadline=$((SECONDS + 60))
    until [[ -n "${xr_name}" ]]; do
        xr_name=$(kubectl get cluster dev-cluster -n crossplane-system \
            -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
        if (( SECONDS > deadline )); then
            error "Claim dev-cluster was never bound to a composite after 60s."
            error "  kubectl describe cluster dev-cluster -n crossplane-system"
            exit 1
        fi
        [[ -n "${xr_name}" ]] || sleep 3
    done
    info "  Composite name: ${xr_name}"

    info "Waiting for XCluster ${xr_name} to be Ready (timeout: ${CLUSTER_XR_TIMEOUT})..."
    kubectl wait "xcluster/${xr_name}" \
        --for=condition=Ready --timeout="${CLUSTER_XR_TIMEOUT}" \
    || {
        error "XCluster ${xr_name} did not become Ready within ${CLUSTER_XR_TIMEOUT}."
        error "Diagnose with:"
        error "  kubectl describe xcluster ${xr_name}"
        error "  kubectl get managed -l crossplane.io/composite=${xr_name}"
        exit 1
    }

    success "Cluster XR ${xr_name} is Ready — kernel structural resources provisioned."

    local mr_count
    mr_count=$(kubectl get managed -l "crossplane.io/composite=${xr_name}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    info "  ${mr_count} managed resource(s) reconciled."

    upsert_gentian_cluster_config
}

# =============================================================================
# seed_secrets_remaining — Seed the KV paths that the Cluster XR does not
# manage: internal/master-password, storage/registry, dns/cloudflare,
# database/cnpg, and other kernel paths.
# Delegates to the existing seed-openbao.sh (uses kv_put_once for safety).
# =============================================================================
seed_secrets_remaining() {
    # seed_secrets() is defined in install.sh (sources seed-openbao.sh).
    # The Cluster XR already wrote the 7 HMAC-derived paths (or observed them
    # if they pre-existed). seed_secrets will skip those via kv_put_once and
    # write only the paths not covered by the Cluster XR.
    seed_secrets
}

# =============================================================================
# Step 10d — Apply root ArgoCD ApplicationSet
#
# gentian-appsets is the "app of apps" that syncs kernel/appsets/ into the
# cluster. Each YAML in that directory becomes an ApplicationSet, driving:
#   - 02-external-secrets: globals-secrets-dev (ESO ExternalSecrets per env)
#   - 08-infra-data:        postgres/mariadb/redis/minio ESO + values ConfigMaps (InfraData XR owns Releases)
#   - 09-suze:              Suze IdP prerequisites (OpenFGA + Keycloak ESO + values)
#
# Prerequisites:
#   - ArgoCD must be installed and the 'gentian' AppProject must exist.
#   - The 'gentian' AppProject is created by apply_cluster_xr (Cluster XR).
#   - seed_secrets_remaining must have run so ESO can sync the globals secrets.
# =============================================================================
bootstrap_root_appset() {
    banner "Step 10d — Bootstrap root ArgoCD ApplicationSet (app-of-apps)"

    export GENTIAN_DEPLOYMENTS_STAGE="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"
    envsubst < "${SCRIPT_DIR}/kernel/bootstrap/root-applicationset.yaml.tmpl" \
        | kubectl apply -f -
    success "gentian-appsets Application applied."

    info "Waiting for gentian-appsets Application to be Synced (up to 2m)..."
    local i=0
    until kubectl get application gentian-appsets -n argocd \
            -o jsonpath='{.status.sync.status}' 2>/dev/null | grep -q "Synced"; do
        echo -n "."
        sleep 5; i=$((i + 5))
        [[ $i -lt 120 ]] || {
            warn "gentian-appsets not yet Synced after 2m — continuing anyway."
            echo ""
            break
        }
    done
    echo ""

    success "Root ApplicationSet bootstrapped — ApplicationSets being deployed."
    info "  Monitor: kubectl get applicationsets -n argocd"
    info "  Apps:    kubectl get applications -n argocd"
}

# =============================================================================
# Step 15: Bootstrap gentian-catalogue ApplicationSet (profile bundles from gentian-apps)
# =============================================================================
bootstrap_appprofiles() {
    install_catalogue_sync
}

# =============================================================================
# Step 11: Install provider-helm
# provider-helm deploys Helm charts as Crossplane Managed Resources (InfraData XR,
# kernel services, tenant apps via compositions).
# =============================================================================
install_provider_helm() {
    banner "Step 11 — Install provider-helm"

    # providers.yaml already contains provider-helm; apply idempotently.
    kubectl apply -f "${SCRIPT_DIR}/crossplane/providers/providers.yaml"
    kubectl apply -f "${SCRIPT_DIR}/crossplane/providers/provider-configs.yaml"

    info "Waiting for provider-helm to become Healthy (up to 3m)..."
    kubectl wait provider/provider-helm \
        --for=condition=Healthy --timeout=180s \
    || {
        error "provider-helm did not become Healthy within 180s."
        error "  kubectl describe provider/provider-helm"
        exit 1
    }

    success "provider-helm Healthy."
}

# =============================================================================
# Step 11b — Apply InfraData XR (shared PostgreSQL, MariaDB, Redis, MinIO)
#
# Provisions kernel data stores via Crossplane InfraData XR.
#
# Prerequisites:
#   - provider-helm Healthy (Step 11)
#   - gentian-infra-data AppSet synced ESO Secrets + values ConfigMaps (wave 8)
#   - charts/infra/packages published on GitHub for the target git branch
#     (run ./scripts/publish-infra-charts.sh and push before install when adding charts)
# =============================================================================
detect_infra_chart_repo() {
    if [[ -n "${INFRA_CHART_REPO:-}" ]]; then
        echo "${INFRA_CHART_REPO}"
        return
    fi
    local branch
    branch="$(git -C "${SCRIPT_DIR}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo develop)"
    if [[ "${branch}" == "HEAD" ]]; then
        branch=develop
    fi
    echo "https://raw.githubusercontent.com/gentian-org/gentian-os/${branch}/charts/infra/packages"
}

verify_infra_chart_index() {
    local repo="$1"
    local index_url="${repo}/index.yaml"
    info "Verifying infra Helm index: ${index_url}"
    local index
    if ! index="$(curl -fsSL "${index_url}" 2>/dev/null)"; then
        error "Could not fetch ${index_url}"
        error "  Publish charts with: ./scripts/publish-infra-charts.sh && git push"
        error "  Or set INFRA_CHART_REPO to a branch that contains redis/minio packages."
        return 1
    fi
    local missing=()
    for chart in postgresql mariadb redis minio; do
        if ! grep -q "^  ${chart}:" <<<"${index}"; then
            missing+=("${chart}")
        fi
    done
    if ((${#missing[@]} > 0)); then
        error "Infra chart repo is missing: ${missing[*]}"
        error "  Index: ${index_url}"
        error "  Redis/MinIO were added on develop — merge/push to develop, or:"
        error "  INFRA_CHART_REPO=https://raw.githubusercontent.com/gentian-org/gentian-os/develop/charts/infra/packages ./install.sh"
        return 1
    fi
    success "Infra Helm index contains postgresql, mariadb, redis, and minio."
}

apply_infra_data_xr() {
    banner "Step 11b — Apply InfraData XR (shared PostgreSQL, MariaDB, Redis, MinIO)"

    local claim="dev-infra-data"
    local timeout="${INFRA_DATA_XR_TIMEOUT:-10m}"
    local chart_repo
    chart_repo="$(detect_infra_chart_repo)"
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    local infra_ns="${INFRA_NAMESPACE:-gentian-infra-${stage}}"

    verify_infra_chart_index "${chart_repo}"

    # Wait for the gentian-infra-data AppSet (kernel/appsets/08-infra-data.yaml,
    # sync wave 8) to sync every ConfigMap/Secret the InfraData composition's
    # Release CRs require via valuesFrom (all optional: false — see
    # crossplane/compositions/infra-data.yaml). provider-helm does not watch
    # ConfigMaps: a Release created before its ConfigMap exists fails once and
    # never retries on its own (needs a manual delete+recreate, e.g.
    # update.sh --reconcile-releases). Waiting here avoids hitting that race
    # at all instead of recovering from it after the fact.
    info "Waiting for gentian-infra-data AppSet prerequisites in ${infra_ns} (up to 3m)..."
    local deadline=$((SECONDS + 180))
    local apps=(
        "infra-postgresql-${stage}" "infra-mariadb-${stage}"
        "infra-redis-${stage}" "infra-minio-${stage}"
    )
    local configmaps=(
        postgresql-base-values "postgresql-${stage}-values"
        mariadb-base-values "mariadb-${stage}-values"
        redis-env-values redis-base-values "redis-${stage}-values"
        minio-env-values minio-base-values "minio-${stage}-values"
    )
    local secrets=(postgresql-sensitive-values mariadb-sensitive-values)
    local app configmap secret ready
    while :; do
        ready=1
        for app in "${apps[@]}"; do
            kubectl get application "${app}" -n argocd \
                -o jsonpath='{.status.sync.status}' 2>/dev/null | grep -q Synced \
                || { ready=0; break; }
        done
        if [[ "${ready}" == "1" ]]; then
            for configmap in "${configmaps[@]}"; do
                kubectl get configmap "${configmap}" -n "${infra_ns}" >/dev/null 2>&1 \
                    || { ready=0; break; }
            done
        fi
        if [[ "${ready}" == "1" ]]; then
            for secret in "${secrets[@]}"; do
                kubectl get secret "${secret}" -n "${infra_ns}" >/dev/null 2>&1 \
                    || { ready=0; break; }
            done
        fi
        [[ "${ready}" == "1" ]] && break
        if (( SECONDS > deadline )); then
            warn "InfraData prerequisites not ready after 3m — refresh gentian-appsets and retry."
            warn "  kubectl get applications -n argocd | grep -E 'infra-(postgresql|mariadb|redis|minio)'"
            warn "  kubectl get configmap,secret -n ${infra_ns}"
            break
        fi
        sleep 5
    done

    info "Applying InfraData claim (${claim})..."
    kubectl apply -f "${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/kernel/claims/infra-data.yaml"

    info "Setting chartRepository to ${chart_repo}..."
    kubectl patch infradata "${claim}" -n crossplane-system --type=merge \
        -p "{\"spec\":{\"chartRepository\":\"${chart_repo}\"}}"

    info "Waiting for InfraData claim to bind to a composite (up to 60s)..."
    local xr_name=""
    local deadline=$((SECONDS + 60))
    until [[ -n "${xr_name}" ]]; do
        xr_name=$(kubectl get infradata "${claim}" -n crossplane-system \
            -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
        if (( SECONDS > deadline )); then
            error "InfraData claim ${claim} was never bound to a composite after 60s."
            error "  kubectl describe infradata ${claim} -n crossplane-system"
            exit 1
        fi
        [[ -n "${xr_name}" ]] || sleep 3
    done
    info "  Composite name: ${xr_name}"

    info "Waiting for XInfraData ${xr_name} to be Ready (timeout: ${timeout})..."
    kubectl wait "xinfradata/${xr_name}" \
        --for=condition=Ready --timeout="${timeout}" \
    || {
        error "XInfraData ${xr_name} did not become Ready within ${timeout}."
        error "  kubectl describe xinfradata ${xr_name}"
        error "  kubectl get managed -l crossplane.io/composite=${xr_name}"
        error "  kubectl describe release.helm.crossplane.io -l crossplane.io/composite=${xr_name}"
        error "Common cause: chart not found — verify ${chart_repo}/index.yaml lists redis and minio."
        exit 1
    }

    success "InfraData XR ${xr_name} is Ready — shared PostgreSQL, MariaDB, Redis, and MinIO provisioned."
}

# =============================================================================
# Step 11c — Kyverno admission controller (Stage 0 MAC)
#
# Deployed by Argo CD via kernel/appsets/05-admission.yaml (sync wave 5–6).
# This step waits for the controller so later workloads are admitted under policy.
# =============================================================================
install_mac_admission() {
    banner "Step 11c — Kyverno admission controller (Stage 0 MAC)"

    info "Kyverno is synced by gentian-appsets (kernel/appsets/05-admission.yaml)."
    info "Waiting for kyverno-admission-controller (up to 5m)..."

    local deadline=$((SECONDS + 300))
    until kubectl get deployment kyverno-admission-controller -n kyverno >/dev/null 2>&1; do
        if (( SECONDS > deadline )); then
            warn "Kyverno deployment not found after 5m — refresh gentian-appsets and retry."
            warn "  kubectl patch application gentian-appsets -n argocd --type merge -p '{\"metadata\":{\"annotations\":{\"argocd.argoproj.io/refresh\":\"hard\"}}}'"
            return 0
        fi
        sleep 5
    done

    kubectl wait deployment/kyverno-admission-controller -n kyverno \
        --for=condition=Available --timeout=300s \
    || {
        warn "Kyverno admission controller did not become Available within 300s."
        warn "  kubectl get pods -n kyverno"
        return 0
    }

    success "Kyverno admission controller is ready."
}

# =============================================================================
# Step 12 — Suze XR (Gentian IdP: Keycloak + OpenFGA, Stage 1)
# =============================================================================
wait_for_crd_established() {
    local crd="$1"
    local timeout_sec="${2:-120}"
    local deadline=$((SECONDS + timeout_sec))
    until kubectl get crd "${crd}" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null \
        | grep -q True; do
        if (( SECONDS > deadline )); then
            error "CRD ${crd} did not become Established within ${timeout_sec}s."
            return 1
        fi
        sleep 3
    done
}

wait_xr_condition_ready() {
    local api_version_kind="$1"  # e.g. xsuze/my-xr
    local timeout_sec="$2"
    local deadline=$((SECONDS + timeout_sec))
    while true; do
        local ready
        ready=$(kubectl get "${api_version_kind}" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
        if [[ "${ready}" == "True" ]]; then
            return 0
        fi
        if (( SECONDS > deadline )); then
            return 1
        fi
        sleep 10
    done
}

# Suze Keycloak uses keycloakx chart service {release}-keycloakx-http (default release gentian-idp-keycloak).
_suze_keycloak_service_name() {
    echo "${GENTIAN_IDP_KEYCLOAK_RELEASE:-gentian-idp-keycloak}-keycloakx-http"
}

_suze_openfga_service_name() {
    echo "${GENTIAN_OPENFGA_RELEASE:-gentian-openfga}"
}

# True when Crossplane Helm releases have live Endpoints (not just MR status=deployed).
_suze_idp_workloads_ready() {
    local ns="platform-kernel"
    local kc_svc fga_svc
    kc_svc=$(_suze_keycloak_service_name)
    fga_svc=$(_suze_openfga_service_name)
    kubectl get svc -n "${ns}" "${kc_svc}" >/dev/null 2>&1 \
        && kubectl get endpoints -n "${ns}" "${kc_svc}" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q . \
        && kubectl get svc -n "${ns}" "${fga_svc}" >/dev/null 2>&1 \
        && kubectl get endpoints -n "${ns}" "${fga_svc}" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q .
}

_reset_suze_ghost_helm_releases() {
    local xr_name="$1"
    warn "Suze XR ${xr_name} reports Ready but IdP Services are missing — resetting Helm Release MRs..."
    while IFS= read -r rel; do
        [[ -z "${rel}" ]] && continue
        warn "  Deleting Release ${rel}..."
        kubectl delete release.helm.crossplane.io/"${rel}" --wait=true --timeout=180s 2>/dev/null || true
    done < <(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${xr_name}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
}

# Heal ghost Suze state and wait for Keycloak + OpenFGA Services (used by Steps 14 and 16).
ensure_suze_idp_workloads() {
    local xr_name="${1:-}"
    local timeout_sec="${2:-600}"
    local ns="platform-kernel"

    if [[ -z "${xr_name}" ]]; then
        xr_name=$(kubectl get suze dev-suze -n crossplane-system \
            -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
    fi
    [[ -n "${xr_name}" ]] || {
        error "Suze composite not found — run Step 12 first."
        return 1
    }

    if _suze_idp_workloads_ready; then
        return 0
    fi

    if [[ "$(kubectl get "xsuze/${xr_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)" == "True" ]]; then
        _reset_suze_ghost_helm_releases "${xr_name}"
    fi

    local kc_svc fga_svc
    kc_svc=$(_suze_keycloak_service_name)
    fga_svc=$(_suze_openfga_service_name)
    info "Waiting for Suze IdP Services (${kc_svc}, ${fga_svc}) in ${ns}..."
    local deadline=$((SECONDS + timeout_sec))
    while (( SECONDS < deadline )); do
        if _suze_idp_workloads_ready; then
            success "Suze IdP workloads are live."
            return 0
        fi
        sleep 10
    done

    error "Suze IdP workloads not ready within ${timeout_sec}s."
    error "  kubectl get release.helm.crossplane.io -l crossplane.io/composite=${xr_name}"
    error "  kubectl get svc,pods -n ${ns} | grep -E 'keycloak|openfga'"
    return 1
}

apply_suze_xr() {
    banner "Step 12 — Apply Suze XR (Secure Universal Zero-trust Environment)"

    local claim="dev-suze"
    local timeout_sec="${SUZE_XR_TIMEOUT_SEC:-1200}"

    info "Waiting for Suze Argo apps + prerequisites in platform-kernel (up to 3m)..."
    local deadline=$((SECONDS + 180))
    until kubectl get application openfga-dev -n argocd -o jsonpath='{.status.sync.status}' 2>/dev/null | grep -q Synced \
        && kubectl get application keycloak-idp-dev -n argocd -o jsonpath='{.status.sync.status}' 2>/dev/null | grep -q Synced \
        && kubectl get configmap openfga-base-values -n platform-kernel >/dev/null 2>&1 \
        && kubectl get secret openfga-sensitive-values -n platform-kernel >/dev/null 2>&1 \
        && kubectl get configmap keycloak-idp-base-values -n platform-kernel >/dev/null 2>&1 \
        && kubectl get secret keycloak-idp-sensitive-values -n platform-kernel >/dev/null 2>&1; do
        if (( SECONDS > deadline )); then
            warn "Suze prerequisites not ready — refresh gentian-appsets and retry."
            warn "  kubectl get applications -n argocd | grep -E 'openfga|keycloak-idp'"
            break
        fi
        sleep 5
    done

    info "Applying Suze claim (${claim})..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/xrds/suze.yaml"
    wait_for_crd_established "xsuze.gentianos.io" 120
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/suze.yaml"
    kubectl apply -f "${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/kernel/claims/suze.yaml"

    info "Waiting for Suze claim to bind (up to 60s)..."
    local xr_name=""
    deadline=$((SECONDS + 60))
    until [[ -n "${xr_name}" ]]; do
        xr_name=$(kubectl get suze "${claim}" -n crossplane-system \
            -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
        if (( SECONDS > deadline )); then
            error "Suze claim ${claim} was never bound to a composite after 60s."
            exit 1
        fi
        [[ -n "${xr_name}" ]] || sleep 3
    done
    info "  Composite name: ${xr_name}"

    if wait_xr_condition_ready "xsuze/${xr_name}" 10 && _suze_idp_workloads_ready; then
        success "Suze XR ${xr_name} is already Ready with live IdP workloads — skipping reinstall."
        return 0
    fi

    if wait_xr_condition_ready "xsuze/${xr_name}" 10 && ! _suze_idp_workloads_ready; then
        _reset_suze_ghost_helm_releases "${xr_name}"
    fi

    # Delete failed Helm Release MRs so the composition reinstalls with updated values.
    local rel state synced
    while IFS= read -r rel; do
        [[ -z "${rel}" ]] && continue
        state=$(kubectl get release.helm.crossplane.io/"${rel}" \
            -o jsonpath='{.status.atProvider.state}' 2>/dev/null || true)
        synced=$(kubectl get release.helm.crossplane.io/"${rel}" \
            -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}' 2>/dev/null || true)
        if [[ "${state}" == "failed" || "${synced}" == "False" ]]; then
            warn "Resetting failed Suze Release ${rel} for clean reinstall..."
            kubectl delete release.helm.crossplane.io/"${rel}" --wait=true --timeout=180s 2>/dev/null || true
        fi
    done < <(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${xr_name}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)

    for hr in gentian-openfga gentian-idp-keycloak; do
        if helm status "${hr}" -n platform-kernel 2>/dev/null | grep -qE 'STATUS: (failed|pending-install|pending-upgrade)'; then
            warn "Uninstalling stuck Helm release ${hr} in platform-kernel..."
            helm uninstall "${hr}" -n platform-kernel --wait --timeout=3m 2>/dev/null || true
        fi
    done

    info "Waiting for XSuze ${xr_name} to be Ready (timeout: ${timeout_sec}s)..."
    wait_xr_condition_ready "xsuze/${xr_name}" "${timeout_sec}" \
    || {
        error "XSuze ${xr_name} did not become Ready within ${timeout_sec}s."
        error "  kubectl describe xsuze ${xr_name}"
        error "  kubectl get managed -l crossplane.io/composite=${xr_name}"
        error "  kubectl get pods -n platform-kernel"
        exit 1
    }

    ensure_suze_idp_workloads "${xr_name}" 300 || exit 1

    if ! verify_keycloak_installation; then
        error "Keycloak installation verification failed."
        exit 1
    fi

    success "Suze XR ${xr_name} is Ready — Gentian IdP (Keycloak + OpenFGA) provisioned."
}


# =============================================================================
# Print Crossplane-aware installation summary
# =============================================================================
print_summary_cp() {
    local xr_name xr_ready mr_count infra_pg_ready infra_mdb_ready infra_redis_ready infra_minio_ready argocd_url argocd_pw

    xr_name=$(kubectl get cluster dev-cluster -n crossplane-system \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
    xr_name="${xr_name:-dev-cluster}"

    xr_ready=$(kubectl get "xcluster/${xr_name}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    mr_count=$(kubectl get managed -l "crossplane.io/composite=${xr_name}" \
        --no-headers 2>/dev/null | wc -l | tr -d ' ')
    infra_pg_ready=$(kubectl get release.helm.crossplane.io/dev-infra-data-postgresql \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    infra_mdb_ready=$(kubectl get release.helm.crossplane.io/dev-infra-data-mariadb \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    infra_redis_ready=$(kubectl get release.helm.crossplane.io/dev-infra-data-redis \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    infra_minio_ready=$(kubectl get release.helm.crossplane.io/dev-infra-data-minio \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    local suze_ready openfga_ready keycloak_ready suze_xr
    suze_ready=$(kubectl get xsuze -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    suze_xr=$(kubectl get suze dev-suze -n crossplane-system \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || echo "dev-suze")
    openfga_rel=$(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${suze_xr}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep openfga | head -1)
    keycloak_rel=$(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${suze_xr}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep keycloak | head -1)
    openfga_ready=$(kubectl get release.helm.crossplane.io/"${openfga_rel}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    keycloak_ready=$(kubectl get release.helm.crossplane.io/"${keycloak_rel}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")

    # Resolve these BEFORE the banner to avoid warnings mid-output.
    argocd_url=$(resolve_argocd_url 2>/dev/null)
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
        -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)

    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     Gentian OS — Bootstrap Complete (new-security)        ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${GREEN}  Kernel domain  : ${KERNEL_DOMAIN:-not set}${NC}"
    echo -e "${GREEN}  Tenancy mode   : ${TENANCY_MODE:-multi}${NC}"
    echo -e "${GREEN}  Kernel realm   : ${KERNEL_REALM:-kernel}${NC}"
    echo -e "${GREEN}  Cluster XR     : ${xr_name} (Ready=${xr_ready}, MRs=${mr_count})${NC}"
    echo -e "${GREEN}  InfraData PG   : dev-infra-data-postgresql (Ready=${infra_pg_ready})${NC}"
    echo -e "${GREEN}  InfraData MDB  : dev-infra-data-mariadb (Ready=${infra_mdb_ready})${NC}"
    echo -e "${GREEN}  InfraData Redis: dev-infra-data-redis (Ready=${infra_redis_ready})${NC}"
    echo -e "${GREEN}  InfraData MinIO: dev-infra-data-minio (Ready=${infra_minio_ready})${NC}"
    echo -e "${GREEN}  Suze XR       : Ready=${suze_ready} (OpenFGA=${openfga_ready}, Keycloak=${keycloak_ready})${NC}"
    echo ""
    echo -e "${GREEN}  Completed      : Steps 14–17 (Stage 1 IdP, authz bridge, portal, app catalogue)${NC}"
    # Portal credentials (MASTER_PASSWORD-derived; same as keycloak-portal-bootstrap Job).
    if [[ -f "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh" ]]; then
        # shellcheck source=scripts/portal-login-bootstrap.sh
        source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
        print_portal_login_summary
    fi
    echo ""
    echo -e "${GREEN}  Inspect authz stack:${NC}"
    echo -e "${GREEN}    kubectl get xsuze,suze -n crossplane-system${NC}"
    echo -e "${GREEN}    kubectl get secret openfga-runtime -n platform-kernel${NC}"
    echo ""
    echo -e "${GREEN}  Inspect Crossplane managed resources:${NC}"
    echo -e "${GREEN}    kubectl get managed -l crossplane.io/composite=${xr_name}${NC}"
    echo -e "${GREEN}    kubectl get release.helm.crossplane.io | grep dev-infra-data${NC}"
    echo ""
    echo -e "${GREEN}  ArgoCD:${NC}"
    echo -e "${GREEN}    URL  : ${argocd_url}${NC}"
    echo -e "${GREEN}    User : admin${NC}"
    echo -e "${GREEN}    Pass : ${argocd_pw}${NC}"
    echo ""
    echo -e "${GREEN}  OpenBao tokens saved to: ${OPENBAO_INIT_FILE}${NC}"
    echo ""
    echo -e "${GREEN}  Gentian OS infra bootstrap complete.${NC}"
    echo ""
}

# =============================================================================
# Stage 1: LLM serving (vLLM / LocalAI serving backend + LiteLLM proxy)
# =============================================================================
install_llm_serving() {
    LLM_SUPPORT="${LLM_SUPPORT:-false}"
    if [[ "${LLM_SUPPORT}" != "true" ]]; then
        info "LLM serving support disabled; skipping deployment."
        return 0
    fi

    banner "Step 13c — Deploying LLM serving stack"
    local env="${ENV:-dev}"
    local ns="platform-kernel"

    # litellm-services.yaml references the litellm-dashboard-sso Secret
    # (GENERIC_CLIENT_ID/SECRET) — ensure it exists even though Step 14
    # (portal bootstrap) runs after this step.
    # shellcheck source=scripts/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
    ensure_litellm_sso_secret >/dev/null

    local manifests_dir="${SCRIPT_DIR}/kernel/services/llm/manifests/${env}"
    kubectl apply -f "${manifests_dir}/llm-services.yaml" -f "${manifests_dir}/externalsecret.yaml"
    render_and_apply_gpu_sharing_manifest "${manifests_dir}"

    GPU_ACCELERATION="${GPU_ACCELERATION:-false}"
    if [[ "${GPU_ACCELERATION}" == "true" ]]; then
        info "Deploying GPU vLLM inference backend..."
        render_and_apply_vllm_gpu_manifest "${manifests_dir}"
    else
        info "Deploying mock inference backend (GPU_ACCELERATION=false) — see vllm-gpu.yaml.tmpl to serve a real model."
        kubectl apply -f "${manifests_dir}/vllm-mock.yaml"
    fi

    info "Waiting for llm-sensitive-values ExternalSecret to sync (up to 60s)..."
    kubectl wait externalsecret/llm-sensitive-values \
        -n "${ns}" --for=condition=Ready --timeout=60s \
    || warn "llm-sensitive-values not yet Ready — it will sync when OpenBao is available."

    ensure_litellm_teams || warn "LiteLLM team sync failed — retry with ./update.sh --llm."
    if [[ "${GPU_ACCELERATION}" == "true" ]]; then
        ensure_litellm_vllm_model || warn "LiteLLM vLLM model sync failed — retry with ./update.sh --llm."
    fi

    success "LLM serving stack deployment complete."
}

# =============================================================================
# main — Crossplane-based bootstrap
# =============================================================================
main_cp() {
    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     Gentian OS — Crossplane Bootstrap                    ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""

    parse_args "$@"

    if [[ "${INSTALL_VERIFY_ONLY:-0}" == "1" ]]; then
        verify_argocd_apps || true
        print_summary_cp
        return 0
    fi

    load_operator_config
    load_creds_cache
    load_install_state
    load_deployments_cluster_settings
    try_load_creds_from_openbao

    # Capture run start so stale-data guards can distinguish resources created
    # during this install attempt from leftovers from previous cycles.
    # IMPORTANT: this must be set AFTER load_install_state and reused across
    # re-runs of a partially-completed install. Otherwise PVCs created by
    # ArgoCD / Crossplane during a previous (aborted) run get reclassified as
    # stale on the next run because their creationTimestamp predates the new
    # run's epoch. Cleared on successful completion (see end of main_cp).
    INSTALL_START_EPOCH="${INSTALL_START_EPOCH:-$(date -u +%s)}"
    export INSTALL_START_EPOCH
    save_install_state

    [[ "${INSTALL_VALIDATE_ONLY:-0}" == "1" ]] && validate_config

    prompt_app_repos
    prompt_credentials
    resolve_kernel_domain_from_claim  # already-bootstrapped cluster: read from its Claim, skip the prompt below
    prompt_kernel_domain
    prompt_network_mode
    prompt_kernel_secrets
    CROSSPLANE_MODE=1 check_prereqs
    _ensure_bao
    scaffold_cluster_deployment  # new cluster only — no-op if already scaffolded

    # ── Crossplane core + providers ──────────────────────────────────────────
    install_crossplane          # Step 0  — Crossplane controller
    install_crossplane_providers  # Step 0b — providers, XRD, Composition

    # ── Cluster infrastructure ───────────────────────────────────────────────
    create_namespaces           # Step 1
    prewarm_cluster             # Step 1b
    install_cert_manager        # Step 2
    install_kernel_cert_resources  # Step 2b — ClusterIssuers
    install_envoy_gateway       # Step 2c — Envoy Gateway (ROUTING_MODE=gateway)
    install_eso                 # Step 3

    # ── ArgoCD + OpenBao bootstrap ────────────────────────────────────────────
    install_argocd              # Step 4
    install_argocd_image_updater  # Step 4b
    bootstrap_transit_app       # Step 5  — transit seal ArgoCD app
    init_openbao_transit        # Step 5b — transit init + auto-unseal Secret
    bootstrap_argocd_apps       # Step 6  — openbao, reloader, cnpg, globals
    init_openbao                # Step 7  — primary OpenBao init (BAO_TOKEN set here)

    # ── Crossplane kernel provisioning ───────────────────────────────────────
    bootstrap_openbao_for_crossplane  # Step 8  — K8s auth + crossplane-write policy
    create_crossplane_secrets         # Step 9  — derived-credential K8s Secrets
    apply_cluster_xr                  # Step 10 — Cluster XR → all kernel MRs
    seed_secrets_remaining            # Step 10b — remaining paths (registry, DNS, etc.)

    # ── Optional TLS wildcard ─────────────────────────────────────────────────
    install_kernel_wildcard     # Step 10c (optional) — wildcard cert (requires CF_API_TOKEN)
    bootstrap_root_appset       # Step 10d — root app-of-apps (minio, redis, mariadb, IAM…)

    # ── Crossplane provider-helm + shared infra ───────────────────────────────
    install_provider_helm       # Step 11  — wait for provider-helm Healthy
    apply_infra_data_xr         # Step 11b — shared PostgreSQL + MariaDB via InfraData XR
    install_mac_admission       # Step 11c — Kyverno admission (Stage 0 MAC)

    # ── Stage 1: OpenFGA + standalone Keycloak + authz bridge ───────────────
    apply_suze_xr               # Step 12  — Gentian IdP (Keycloak + OpenFGA) via Suze XR
    install_gentian_os_operator # Step 13  — operator with authz bridge + Cloudflare tunnel
    wait_for_gateway_platform || true
    install_kernel_mail         # Step 13b — MAIL_SERVICE_MODE (external SMTP or Postfix)
    install_llm_serving         # Step 13c — Deploy LLM serving stack (vLLM/LocalAI + LiteLLM)
    # shellcheck source=scripts/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
    configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."
    install_portal_login        # Step 14 — portal OIDC login dogfood

    # ── App catalogue + CLI (required for kubectl gentian apps install) ─────
    bootstrap_appprofiles       # Step 15 — AppProfile CRs from gentian-apps repo
    install_app_catalogue       # Step 16 — kubectl-gentian plugin + AppCatalogue CRD

    # ── Optional kernel mail / gateway (uncomment when MAIL_SERVICE_MODE=kernel) ─
    # deploy_kernel_mail_services
    # apply_kernel_gateway_overlays || true
    # wait_for_gateway_platform || true
    # verify_argocd_apps || true
    # verify_keycloak_iframe_policy || true
    # configure_github_actions_secrets   # CI_BOT_PAT → gentian-os Actions secrets

    success "Bootstrap complete — Stage 1 IdP (Keycloak + OpenFGA), authz bridge, and portal login are live."
    unset INSTALL_START_EPOCH
    save_install_state
    print_summary_cp
}

run_operator_only() {
    load_operator_config
    load_creds_cache
    load_install_state
    load_deployments_cluster_settings
    try_load_creds_from_openbao
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        error "KERNEL_DOMAIN not set after loading install.env and cluster-settings.env."
        error "  Set GENTIAN_DEPLOYMENTS_CLUSTER in install.env (e.g. test)."
        error "  Ensure gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env defines KERNEL_DOMAIN."
        exit 1
    fi
    info "Using KERNEL_DOMAIN=${KERNEL_DOMAIN}"
    install_gentian_os_operator
    wait_for_gateway_platform || true
}

run_portal_only() {
    load_operator_config
    load_creds_cache
    load_install_state
    load_deployments_cluster_settings
    try_load_creds_from_openbao
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        error "KERNEL_DOMAIN not set after loading install.env and cluster-settings.env."
        error "  Set GENTIAN_DEPLOYMENTS_CLUSTER in install.env (e.g. test)."
        error "  Ensure gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env defines KERNEL_DOMAIN."
        exit 1
    fi
    [[ -n "${MASTER_PASSWORD:-}" ]] || { error "MASTER_PASSWORD not set — add to install.secrets.env."; exit 1; }
    [[ ${#MASTER_PASSWORD} -ge 16 ]] || { error "MASTER_PASSWORD is too weak. It must be at least 16 characters long."; exit 1; }
    # Try to read existing derivation salt from OpenBao
    local existing_salt
    existing_salt=$(bao kv get -mount=secret -field=salt gentian-os/kernel/internal/master-password 2>/dev/null || true)
    if [[ -n "${existing_salt}" ]]; then
        export DERIVATION_SALT="${existing_salt}"
    fi
    # shellcheck source=scripts/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
    install_portal_login
}

case "${1:-}" in
    --operator-only)
        run_operator_only
        exit 0
        ;;
    --portal-only)
        run_portal_only
        exit 0
        ;;
esac

main_cp "$@"
