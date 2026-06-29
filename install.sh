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
# Usage:
#   ./install.sh
#   ./install.sh --validate          # validate config only, no cluster changes
#   ./install.sh --no-cluster-infra  # skip cert-manager/CNPG/reloader
#
# Required environment variables: same as install.sh (see getting-started.md)
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load all helper functions from scripts/install-lib.sh without running its main().
export GENTIAN_INSTALL_LIB_ONLY=1
# shellcheck source=scripts/install-lib.sh
source "${SCRIPT_DIR}/scripts/install-lib.sh"
unset GENTIAN_INSTALL_LIB_ONLY

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
    helm template crossplane crossplane-stable/crossplane \
        --version "${CROSSPLANE_VERSION}" \
        --namespace "${CROSSPLANE_NAMESPACE}" \
        --include-crds \
        | kubectl apply -f - >/dev/null

    # Some chart packaging modes do not include CRDs in Helm output. Ensure the
    # required package CRDs are explicitly applied from upstream release assets.
    local crossplane_minor="${CROSSPLANE_VERSION%.*}"
    for crd in providers providerrevisions functions functionrevisions deploymentruntimeconfigs; do
        kubectl apply -f "https://raw.githubusercontent.com/crossplane/crossplane/release-${crossplane_minor}/cluster/crds/pkg.crossplane.io_${crd}.yaml" >/dev/null 2>&1 || true
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
    banner "Crossplane 0 — Install Crossplane core"

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
    banner "Crossplane 0b/0c — Providers, XRD, Composition"

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
    banner "Step 10 — Bootstrap OpenBao auth for Crossplane"

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    export VAULT_ADDR="http://${BAO_SVC_IP}:8200"

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

    # ── 3c. app-init policy ───────────────────────────────────────────────────
    # App init Jobs (Crossplane Compositions) authenticate via K8s SA JWT and
    # use this policy to provision per-tenant/app credentials in OpenBao on
    # first app install (ldap, s3, database).
    bao policy write app-init - <<POLICY
path "${_kv_mount}/data/gentian-os/kernel/internal/master-password"   { capabilities = ["read"] }
path "${_kv_mount}/data/gentian-os/kernel/identity/nubus"            { capabilities = ["read"] }
path "${_kv_mount}/data/gentian-os/kernel/database/cnpg"             { capabilities = ["read"] }
path "${_kv_mount}/data/gentian-os/kernel/storage/minio"             { capabilities = ["read"] }
path "${_kv_mount}/metadata/gentian-os/kernel/database/cnpg"         { capabilities = ["read"] }
path "${_kv_mount}/data/gentian-os/tenants/+/apps/+/ldap"            { capabilities = ["create", "read", "update"] }
path "${_kv_mount}/metadata/gentian-os/tenants/+/apps/+/ldap"        { capabilities = ["read"] }
path "${_kv_mount}/data/gentian-os/tenants/+/apps/+/s3"              { capabilities = ["create", "read", "update"] }
path "${_kv_mount}/metadata/gentian-os/tenants/+/apps/+/s3"          { capabilities = ["read"] }
path "${_kv_mount}/data/gentian-os/tenants/+/apps/+/database"        { capabilities = ["create", "read", "update"] }
path "${_kv_mount}/metadata/gentian-os/tenants/+/apps/+/database"    { capabilities = ["read"] }
POLICY
    success "app-init policy written."

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

    # app-init: short-lived tokens for Composition init Jobs running in tenant
    # namespaces. Wildcard namespace binding allows Jobs in any tenant namespace.
    bao write auth/kubernetes/role/app-init \
        bound_service_account_names=app-init \
        bound_service_account_namespaces="*" \
        token_policies=app-init \
        token_ttl=300 \
        token_max_ttl=600
    success "app-init K8s auth role created."

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
            http_code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
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
    banner "Step 11 — Create derived-credential Secrets for Cluster XR"

    # Same derivation as seed-openbao.sh and crossplane/functions/derive-secrets/derive.py
    _derive() { echo -n "${1}:${2}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" | awk '{print $2}'; }
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
        --dry-run=client -o yaml | kubectl apply -f -
    success "  gentian-os-master-password"

    # ── database/postgresql ───────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-database-postgresql" \
        "$(jq -nc \
            --arg a "$(_derive postgres postgres_user)" \
            --arg b "$(_derive postgres keycloak_user)" \
            --arg c "$(_derive postgres keycloak_extensions_user)" \
            --arg d "$(_derive postgres selfservice_user)" \
            --arg e "$(_derive postgres authsession_user)" \
            --arg f "$(_derive postgres guardianmanagementapi_user)" \
            --arg g "$(_derive postgres notificationsapi_user)" \
            --arg h "$(_derive postgres openfga_user)" \
            '{postgres_password:$a,keycloak_user_password:$b,keycloak_extensions_user_password:$c,selfservice_user_password:$d,authsession_user_password:$e,guardianmanagementapi_user_password:$f,notificationsapi_user_password:$g,openfga_user_password:$h}')"

    # ── database/mariadb ──────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-database-mariadb" \
        "$(jq -nc \
            --arg a "$(_derive mariadb root_password)" \
            --arg b "$(_derive mariadb openxchange_user)" \
            '{root_password:$a,openxchange_password:$b}')"

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
            --arg c "$(_derive minio ums_user)" \
            --arg h "$(_derive minio migrations_user)" \
            --arg i "$(_derive minio dovecot_user)" \
            '{root_user:$a,root_password:$b,ums_password:$c,migrations_password:$h,dovecot_password:$i}')"

    # ── identity/nubus ────────────────────────────────────────────────────────
    # shellcheck disable=SC2016
    _kv_secret "gentian-os-kernel-identity-nubus" \
        "$(jq -nc \
            --arg mp "${MASTER_PASSWORD}" \
            --arg a  "$(_derive nubus Administrator)" \
            --arg b  "$(_derive "cn=admin" ldap)" \
            --arg c  "$(_derive keycloak adminPassword)" \
            --arg e  "$(_nats api nats)" \
            --arg f  "$(_nats dispatcher nats)" \
            --arg g  "$(_nats prefill nats)" \
            --arg h  "$(_nats udmListener nats)" \
            --arg i  "$(_nats udmTransformer nats)" \
            --arg j  "$(_derive minio ums_user)" \
            --arg k  "$(_derive postgres selfservice_user)" \
            --arg l  "$(_derive postgres authsession_user)" \
            --arg m  "$(_derive postgres keycloak_user)" \
            --arg n  "$(_derive postgres keycloak_extensions_user)" \
            --arg o  "$(_derive postgres guardianmanagementapi_user)" \
            --arg p  "$(_derive postgres notificationsapi_user)" \
            --arg q  "$(_derive nubus ldapsearch_keycloak)" \
            --arg s  "$(_derive nubus ldapsearch_dovecot)" \
            --arg v  "$(_derive nubus ldapsearch_postfix)" \
            --arg y  "$(_derive centralnavigation api_key)" \
            --arg z  "$(_derive portal-consumer provisioning-api)" \
            --arg z2 "$(_derive selfservice-consumer provisioning-api)" \
            --arg z3 "$(_derive smtp password)" \
            '{master_password:$mp,admin_password:$a,ldap_admin_password:$b,keycloak_admin_password:$c,nats_api_password:$e,nats_dispatcher_password:$f,nats_prefill_password:$g,nats_udm_listener_password:$h,nats_udm_transformer_password:$i,minio_ums_secret_access_key:$j,pg_selfservice_password:$k,pg_authsession_password:$l,pg_keycloak_password:$m,pg_keycloak_extensions_password:$n,pg_guardian_password:$o,pg_notifications_password:$p,ldapsearch_keycloak:$q,ldapsearch_dovecot:$s,ldapsearch_postfix:$v,portal_shared_secret:$y,portal_consumer_api_password:$z,selfservice_consumer_api_password:$z2,smtp_password:$z3}')"

    # ── identity/keycloak-bootstrap ───────────────────────────────────────────
    _kv_secret "gentian-os-kernel-identity-keycloak-bootstrap" \
        "$(jq -nc \
            --arg a "$(_derive keycloak adminPassword)" \
            --arg b "$(_derive keycloak intercom_client_secret)" \
            '{admin_password:$a,intercom_client_secret:$b}')"

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
            --arg user "${OD_SMTP_RELAY_USERNAME:-}" \
            --arg pass "${OD_SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"

    # ── mail/dovecot (HMAC-derived; only active when MAIL_SERVICE_MODE=kernel) ─
    # The Cluster XR creates a SecretV2 MR for this path and will seed OpenBao
    # on first apply. The doveadm_password shares its derivation namespace with
    # the minio secret for cross-service derivation consistency.
    _kv_secret "gentian-os-kernel-mail-dovecot" \
        "$(jq -nc \
            --arg doveadm "$(_derive minio dovecot_user)" \
            --arg oidc "$(_derive dovecot oidcClientSecret)" \
            '{doveadm_password:$doveadm,oidc_client_secret:$oidc}')"

    success "All 9 input Secrets applied to ${CROSSPLANE_NAMESPACE}."
}

# =============================================================================
# Crossplane step 12 — Apply Cluster claim and wait for Ready.
# The Cluster XR creates all 19 kernel MRs via provider-vault and
# provider-kubernetes. managementPolicies: [Observe,Create] on KV seeds
# ensures existing paths seeded by prior install runs are never overwritten.
# =============================================================================
apply_cluster_xr() {
    banner "Step 12 — Apply Cluster XR (kernel structural provisioning)"

    # Derive defaults for template variables not already set.
    # LDAP_BASE_DN: dc= decomposition of KERNEL_DOMAIN (e.g. desk.example.com → dc=desk,dc=example,dc=com)
    local _dn_parts
    _dn_parts=$(echo "${KERNEL_DOMAIN}" | tr '.' '\n' | sed 's/^/dc=/' | paste -sd ',')
    export LDAP_BASE_DN="${LDAP_BASE_DN:-${_dn_parts}}"
    export LETSENCRYPT_EMAIL="${LETSENCRYPT_EMAIL:-admin@${KERNEL_DOMAIN}}"
    export OPENBAO_SERVER="${OPENBAO_SERVER:-http://openbao.openbao.svc.cluster.local:8200}"
    export INGRESS_CLASS_NAME="${INGRESS_CLASS_NAME:-nginx}"
    export KV_MOUNT="${KV_MOUNT:-secret}"
    export KERNEL_REALM="${KERNEL_REALM:-kernel}"

    info "Applying Cluster claim (kernelDomain=${KERNEL_DOMAIN})..."
    envsubst < "${SCRIPT_DIR}/crossplane/claims/dev-cluster.yaml.tmpl" \
        | kubectl apply -f -

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
# database/cnpg, and app-level paths (nextcloud, intercom, etc.).
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
# Step 12d — Apply root ArgoCD ApplicationSet
#
# gentian-appsets is the "app of apps" that syncs kernel/appsets/ into the
# cluster. Each YAML in that directory becomes an ApplicationSet, driving:
#   - 02-external-secrets: globals-secrets-dev (ESO ExternalSecrets per env)
#   - 08-infra-data:        postgres/mariadb/redis/minio ESO + values ConfigMaps (InfraData XR owns Releases)
#   - 09-suze:              Suze IdP prerequisites (OpenFGA + Keycloak ESO + values)
#   OpenDesk ApplicationSets live in kernel/appsets/disabled/ on feat/new-security.
#
# Prerequisites:
#   - ArgoCD must be installed and the 'gentian' AppProject must exist.
#   - The 'gentian' AppProject is created by apply_cluster_xr (Cluster XR).
#   - seed_secrets_remaining must have run so ESO can sync the globals secrets.
# =============================================================================
bootstrap_root_appset() {
    banner "Step 12d — Bootstrap root ArgoCD ApplicationSet (app-of-apps)"

    kubectl apply -f "${SCRIPT_DIR}/kernel/bootstrap/root-applicationset.yaml"
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
# Step 15c: Bootstrap gentian-catalogue ApplicationSet (profile bundles from gentian-apps)
# =============================================================================
bootstrap_appprofiles() {
    install_catalogue_sync
}

# =============================================================================
# Step 13: Install provider-helm
# provider-helm deploys Helm charts into the local cluster. It replaces the
# legacy Pattern B approach for secrets-hostile charts.
# =============================================================================
install_provider_helm() {
    banner "Step 13 — Install provider-helm (Pattern B chart deployments)"

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
# Step 13b — Apply InfraData XR (shared PostgreSQL, MariaDB, Redis, MinIO)
#
# Provisions kernel data stores via Crossplane composition instead of
# Argo-synced Helm ApplicationSets.
#
# Prerequisites:
#   - provider-helm Healthy (Step 13)
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
        error "  Redis/MinIO were added on feat/new-security — merge/push to develop, or:"
        error "  INFRA_CHART_REPO=https://raw.githubusercontent.com/gentian-org/gentian-os/feat/new-security/charts/infra/packages ./install.sh"
        return 1
    fi
    success "Infra Helm index contains postgresql, mariadb, redis, and minio."
}

apply_infra_data_xr() {
    banner "Step 13b — Apply InfraData XR (shared PostgreSQL, MariaDB, Redis, MinIO)"

    local env="${ENV:-dev}"
    local claim="dev-infra-data"
    local timeout="${INFRA_DATA_XR_TIMEOUT:-10m}"
    local chart_repo
    chart_repo="$(detect_infra_chart_repo)"

    verify_infra_chart_index "${chart_repo}"

    # Legacy Pattern B Release CRs shared the Helm release name as the MR name.
    # Remove them before the InfraData composition creates new MRs with the
    # same crossplane.io/external-name (opendesk-postgresql-{env}, etc.).
    for rel in "opendesk-postgresql-${env}" "opendesk-mariadb-${env}" "redis-${env}" "minio-${env}"; do
        if ! kubectl get release.helm.crossplane.io/"${rel}" >/dev/null 2>&1; then
            continue
        fi
        local composite
        composite=$(kubectl get release.helm.crossplane.io/"${rel}" \
            -o jsonpath='{.metadata.labels.crossplane\.io/composite}' 2>/dev/null || true)
        [[ -n "${composite}" ]] && continue
        warn "Removing legacy infra Release CR ${rel} (superseded by InfraData XR)..."
        kubectl delete release.helm.crossplane.io/"${rel}" --timeout=180s 2>/dev/null || true
    done

    info "Applying InfraData claim (${claim})..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/claims/dev-infra-data.yaml"

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
# Step 13c — Kyverno admission controller (Stage 0 MAC)
#
# Deployed by Argo CD via kernel/appsets/05-admission.yaml (sync wave 5–6).
# This step waits for the controller so later workloads are admitted under policy.
# =============================================================================
install_mac_admission() {
    banner "Step 13c — Kyverno admission controller (Stage 0 MAC)"

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
# Step 14 — Suze XR (Gentian IdP: Keycloak + OpenFGA, Stage 1)
# =============================================================================
retire_legacy_authz_idp_xr() {
    # Legacy AuthzIdp claim (renamed to Suze in feat/new-security).
    if kubectl get crd authzidps.gentianos.io >/dev/null 2>&1 \
        && kubectl get authzidp dev-authz-idp -n crossplane-system >/dev/null 2>&1; then
        warn "Retiring legacy AuthzIdp claim (renamed to Suze)..."
        kubectl delete authzidp dev-authz-idp -n crossplane-system --timeout=180s 2>/dev/null || true
    fi

    # Orphaned Helm Release MRs from the deleted AuthzIdp composite.
    local rel
    while IFS= read -r rel; do
        [[ -z "${rel}" ]] && continue
        if kubectl get release.helm.crossplane.io/"${rel}" \
            -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null | grep -q .; then
            warn "Clearing finalizer on terminating legacy Release ${rel}..."
            kubectl patch release.helm.crossplane.io/"${rel}" \
                -p '{"metadata":{"finalizers":null}}' --type=merge 2>/dev/null || true
            continue
        fi
        warn "Removing orphaned legacy Release ${rel}..."
        kubectl delete release.helm.crossplane.io/"${rel}" --wait=true --timeout=180s 2>/dev/null || true
    done < <(kubectl get release.helm.crossplane.io -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | grep -E 'dev-authz-idp' || true)

    for hr in gentian-openfga gentian-idp-keycloak; do
        if helm status "${hr}" -n platform-kernel >/dev/null 2>&1; then
            warn "Uninstalling legacy Helm release ${hr} before Suze reinstall..."
            helm uninstall "${hr}" -n platform-kernel --wait --timeout=3m 2>/dev/null || true
        fi
    done

    local deadline=$((SECONDS + 120))
    while kubectl get xauthzidp -o name 2>/dev/null | grep -q authz-idp; do
        if (( SECONDS > deadline )); then
            warn "Legacy XAuthzIdp still present after 2m — continuing."
            break
        fi
        sleep 5
    done
}

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
        error "Suze composite not found — run Step 14 first."
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
    banner "Step 14 — Apply Suze XR (Secure Universal Zero-trust Environment)"

    local claim="dev-suze"
    local timeout_sec="${SUZE_XR_TIMEOUT_SEC:-1200}"

    retire_legacy_authz_idp_xr

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
    kubectl apply -f "${SCRIPT_DIR}/crossplane/claims/dev-suze.yaml"

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

    success "Suze XR ${xr_name} is Ready — Gentian IdP (Keycloak + OpenFGA) provisioned."
}

install_stage1_operator() {
    banner "Step 15 — gentian-os operator (Stage 1 authz bridge)"

    local env="${ENV:-dev}"
    local chart_dir="${SCRIPT_DIR}/charts/gentian-os"
    local ns="gentian-system"
    local infra_ns="gentian-infra-${env}"

    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        kubectl create namespace "$ns"
    fi

    local openfga_token=""
    if kubectl get secret openfga-sensitive-values -n platform-kernel >/dev/null 2>&1; then
        openfga_token=$(kubectl get secret openfga-sensitive-values -n platform-kernel \
            -o jsonpath='{.data.sensitive-values\.yaml}' 2>/dev/null | base64 -d 2>/dev/null \
            | grep -A1 'keys:' | tail -1 | sed 's/.*"\([^"]*\)".*/\1/' || true)
    fi

    kubectl apply -f "${chart_dir}/crds"

    adopt_gentian_os_helm_preflight "$ns"

    # CI publishes ghcr.io/gentian-org/gentian-os:develop on every develop push (see .github/workflows/ci.yaml).
    local operator_tag="${GENTIAN_OS_IMAGE_TAG:-develop}"
    info "Using gentian-os operator image ghcr.io/gentian-org/gentian-os:${operator_tag} (CI)."

    helm upgrade --install gentian-os "$chart_dir" \
        --namespace "$ns" \
        --set openbao.address="http://openbao.openbao.svc.cluster.local:8200" \
        --set kernelDomain="${KERNEL_DOMAIN}" \
        --set tenancyMode="${TENANCY_MODE:-multi}" \
        --set routingMode="${ROUTING_MODE:-gateway}" \
        --set kernelRealm="${KERNEL_REALM:-kernel}" \
        --set authzBridge.enabled=true \
        --set authzBridge.openfgaURL="http://gentian-openfga.platform-kernel.svc.cluster.local:8080" \
        --set "authzBridge.openfgaToken=${openfga_token}" \
        --set infraNamespace="platform-kernel" \
        --set "servicesNamespace=platform-kernel" \
        --set "kernelServices.keycloakInternalURL=http://gentian-idp-keycloak-keycloakx-http.platform-kernel.svc.cluster.local:8080/auth" \
        --set "image.tag=${operator_tag}" \
        --set "image.pullPolicy=${GENTIAN_OS_IMAGE_PULL_POLICY:-Always}" \
        --wait --timeout 5m

    success "gentian-os operator installed with AUTHZ_BRIDGE_ENABLED."
    info "OpenFGA runtime secret: kubectl get secret openfga-runtime -n platform-kernel"
}

# =============================================================================
# Step 14: Deploy Nubus via provider-helm (Pattern B migration)
#
# Creates:
#   - gentian-dev + gentian-infra-dev namespaces
#   - registry-credentials-helm Secret (crossplane-system) for OCI chart pull
#   - registry-credentials imagePullSecret (gentian-dev) for pod image pull
#   - nubus-base-values + nubus-dev-values ConfigMaps (non-sensitive values)
#   - nubus-dev-udm-listener-nats-patch ConfigMap (NATS subject bug workaround)
#     These are seeded by install.sh on first boot; ArgoCD (nubus-manifests-dev)
#     manages them going forward via Kustomize in kernel/services/nubus/manifests/dev/.
#   - ExternalSecrets: nubus-credentials + nubus-sensitive-values (via ESO)
#   - provider-helm Release CR (nubus-dev)
# =============================================================================
deploy_nubus() {
    banner "Step 14 — Deploy Nubus via provider-helm"

    local ns="gentian-${ENV:-dev}"
    local infra_ns="gentian-infra-${ENV:-dev}"
    local release_name="nubus-${ENV:-dev}"
    local install_start_epoch="${INSTALL_START_EPOCH:-0}"

    # Return lines: <pvc_name>\t<reason>
    # reason is one of: pvc-created-before-install, pv-created-before-install
    # This avoids false positives when ArgoCD app-of-apps creates fresh PVCs
    # during the same install run.
    _find_stale_pvcs() {
        local scan_ns="$1"
        local exclude_regex="$2"
        local cutoff=$((install_start_epoch - 15))
        local pvc_list pvc pvc_epoch pvc_ts pv pv_epoch pv_ts reason

        pvc_list=$(kubectl get pvc -n "${scan_ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
        [[ -z "${pvc_list}" ]] && return 0

        while IFS= read -r pvc; do
            [[ -z "${pvc}" ]] && continue
            [[ -n "${exclude_regex}" && "${pvc}" =~ ${exclude_regex} ]] && continue

            reason=""
            pvc_ts=$(kubectl get pvc "${pvc}" -n "${scan_ns}" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)
            pvc_epoch=0
            [[ -n "${pvc_ts}" ]] && pvc_epoch=$(date -d "${pvc_ts}" +%s 2>/dev/null || echo 0)
            if (( install_start_epoch > 0 && pvc_epoch > 0 && pvc_epoch < cutoff )); then
                reason="pvc-created-before-install"
            fi

            pv=$(kubectl get pvc "${pvc}" -n "${scan_ns}" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
            if [[ -n "${pv}" ]]; then
                pv_ts=$(kubectl get pv "${pv}" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)
                pv_epoch=0
                [[ -n "${pv_ts}" ]] && pv_epoch=$(date -d "${pv_ts}" +%s 2>/dev/null || echo 0)
                if (( install_start_epoch > 0 && pv_epoch > 0 && pv_epoch < cutoff )); then
                    if [[ -n "${reason}" ]]; then
                        reason="${reason},pv-created-before-install"
                    else
                        reason="pv-created-before-install"
                    fi
                fi
            fi

            [[ -n "${reason}" ]] && printf '%s\t%s\n' "${pvc}" "${reason}"
        done <<< "${pvc_list}"
        # Always return 0: under set -e, an exit code of 1 from the loop's
        # last `[[ -n ... ]] && printf` (when the final PVC is non-stale) would
        # propagate through $(...) and abort the caller silently.
        return 0
    }

    # ── Namespaces ────────────────────────────────────────────────────────────
    # Guard: if a previous uninstall is still in progress, the namespace may be
    # Terminating. Applying into a Terminating namespace reuses existing PVCs
    # (incl. LDAP data), defeating a clean reinstall. Wait up to 120s.
    for _guard_ns in "${ns}" "${infra_ns}"; do
        local _deadline=$(( SECONDS + 120 ))
        while [[ "$(kubectl get namespace "${_guard_ns}" \
                -o jsonpath='{.status.phase}' 2>/dev/null)" == "Terminating" ]]; do
            if (( SECONDS > _deadline )); then
                error "Namespace ${_guard_ns} is still Terminating after 120s."
                error "Run: kubectl delete namespace ${_guard_ns} --force --grace-period=0"
                exit 1
            fi
            info "  Waiting for ${_guard_ns} to finish terminating..."
            sleep 5
        done
    done

    info "Creating namespaces ${ns} and ${infra_ns}..."
    kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f -
    kubectl create namespace "${infra_ns}" --dry-run=client -o yaml | kubectl apply -f -

    # ── Registry credentials ──────────────────────────────────────────────────
    info "Creating registry-credentials-helm Secret in ${CROSSPLANE_NAMESPACE}..."
    kubectl create secret generic registry-credentials-helm \
        -n "${CROSSPLANE_NAMESPACE}" \
        --from-literal=username="${OD_PRIVATE_REGISTRY_USERNAME}" \
        --from-literal=password="${OD_PRIVATE_REGISTRY_PASSWORD}" \
        --dry-run=client -o yaml | kubectl apply -f -

    info "Creating registry-credentials imagePullSecret in ${ns}..."
    kubectl create secret docker-registry registry-credentials \
        -n "${ns}" \
        --docker-server="registry.opencode.de" \
        --docker-username="${OD_PRIVATE_REGISTRY_USERNAME}" \
        --docker-password="${OD_PRIVATE_REGISTRY_PASSWORD}" \
        --dry-run=client -o yaml | kubectl apply -f -

    # ── Non-sensitive values ConfigMaps ───────────────────────────────────────
    # Seeded here for install sequencing; ArgoCD owns them after first sync.
    info "Creating nubus values ConfigMaps in ${ns}..."
    kubectl create configmap nubus-base-values \
        -n "${ns}" \
        --from-file=values.yaml="${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/values/_base.yaml" \
        --dry-run=client -o yaml | kubectl apply -f -
    kubectl create configmap nubus-dev-values \
        -n "${ns}" \
        --from-file=values.yaml="${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/values/dev.yaml" \
        --dry-run=client -o yaml | kubectl apply -f -
    if [[ "${ROUTING_MODE:-gateway}" == "gateway" ]]; then
        info "Creating nubus gateway values ConfigMap (ROUTING_MODE=gateway)..."
        kubectl create configmap nubus-gateway-values \
            -n "${ns}" \
            --from-file=values.yaml="${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/values/gateway.yaml" \
            --dry-run=client -o yaml | kubectl apply -f -
    fi

    # ── NATS subject patch ConfigMap ──────────────────────────────────────────
    # Fixes LDAP_SUBJECT mismatch between udm-listener and udm-transformer
    # images in nubus 1.16.0. Referenced by nubusUdmListener.extraVolumes.
    info "Creating ${release_name}-udm-listener-nats-patch ConfigMap in ${ns}..."
    kubectl create configmap "${release_name}-udm-listener-nats-patch" \
        -n "${ns}" \
        --from-file=mq_adapter_nats.py="${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/patches/mq_adapter_nats.py" \
        --dry-run=client -o yaml | kubectl apply -f -

    # ── Multi-tenant LDAP ACL patch ConfigMap ─────────────────────────────────
    # Adds cn=Tenant Admins to the cn=temporary ACL rules so tenant admins can
    # provision users (UID lock objects). Referenced by nubusLdapServer.extraVolumes.
    info "Creating ${release_name}-ldap-gentian-acl ConfigMap in ${ns}..."
    kubectl create configmap "${release_name}-ldap-gentian-acl" \
        -n "${ns}" \
        --from-file=92-gentian-tenant-acl.sh="${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/patches/92-gentian-tenant-acl.sh" \
        --dry-run=client -o yaml | kubectl apply -f -

    # ── Pre-flight: abort if stale data PVCs exist ───────────────────────────
    # Nubus StatefulSets (LDAP, UDM listener, portal-consumer, …) bind to
    # volumeClaimTemplate PVCs by name. Helm never deletes these PVCs on
    # uninstall. If they survive from a previous installation the new
    # StatefulSets silently reuse the old volumes, inheriting old users, old
    # LDAP passwords, and expired SSL certificates. Abort loudly instead.
    local _stale_pvcs _stale_list
    _stale_pvcs=$(_find_stale_pvcs "${ns}" "^nats-data-${release_name}-provisioning-nats-0$")
    if [[ -n "${_stale_pvcs}" ]]; then
        error "Stale PVCs detected in ${ns} — aborting to avoid installing on old data:"
        while IFS=$'\t' read -r pvc reason; do
            [[ -z "${pvc}" ]] && continue
            _stale_list+="|${pvc}|${reason}|\n"
        done <<< "${_stale_pvcs}"
        printf '%b' "${_stale_list}" | sed 's/^/    /' >&2 || true
        error ""
        error "These PVCs are from a previous installation (LDAP data, SSL certs, etc.)."
        error "Clean them up first, then re-run install.sh:"
        error "    ./uninstall.sh -f && ./install.sh"
        exit 1
    fi
    _stale_list=""
    _stale_pvcs=$(_find_stale_pvcs "${infra_ns}" "")
    if [[ -n "${_stale_pvcs}" ]]; then
        error "Stale PVCs detected in ${infra_ns} — aborting to avoid installing on old data:"
        while IFS=$'\t' read -r pvc reason; do
            [[ -z "${pvc}" ]] && continue
            _stale_list+="|${pvc}|${reason}|\n"
        done <<< "${_stale_pvcs}"
        printf '%b' "${_stale_list}" | sed 's/^/    /' >&2 || true
        error ""
        error "These PVCs are from a previous installation (postgres, MariaDB, MinIO, …)."
        error "Clean them up first, then re-run install.sh:"
        error "    ./uninstall.sh -f && ./install.sh"
        exit 1
    fi

    # ── Pre-flight: clear stale NATS consumer state ──────────────────────────
    # The provisioning register-consumers job fails with 409 if NATS retained
    # consumer registrations from a previous interrupted install/uninstall.
    # Delete the NATS PVC (only when NATS is not running) so NATS starts with
    # clean JetStream state and consumer registration always succeeds with 201.
    local nats_pvc="nats-data-${release_name}-provisioning-nats-0"
    local _stale_nats=0
    if kubectl get pvc "${nats_pvc}" -n "${ns}" >/dev/null 2>&1; then
        # Use jsonpath to reliably read the pod phase; --field-selector is
        # ignored by kubectl when a specific resource name is also given.
        local _nats_phase
        _nats_phase=$(kubectl get pod "${release_name}-provisioning-nats-0" \
            -n "${ns}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        if [[ "$_nats_phase" != "Running" ]]; then
            info "Deleting stale NATS PVC (consumer state from previous install)..."
            # --wait=false: mark for deletion and return immediately; the PVC
            # will be reclaimed once the old NATS pod (if any) fully terminates.
            kubectl delete pvc "${nats_pvc}" -n "${ns}" --wait=false 2>/dev/null || true
            _stale_nats=1
        else
            info "NATS pod is Running; skipping PVC deletion (healthy install)."
        fi
    fi
    # Remove any leftover failed register-consumers job so Helm creates it fresh.
    # Match any revision suffix (-1, -2, …) to cover manual helm upgrade remnants.
    kubectl get jobs -n "${ns}" --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null \
        | grep "^${release_name}-provisioning-register-consumers-" \
        | xargs -r kubectl delete job -n "${ns}" --ignore-not-found=true 2>/dev/null || true

    # ── ExternalSecrets (ESO → OpenBao → K8s Secrets) ────────────────────────
    info "Applying nubus ExternalSecrets..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/apps/nubus/externalsecrets.yaml"

    info "Waiting for ESO to sync nubus-credentials (up to 60s)..."
    kubectl wait externalsecret/nubus-credentials \
        -n "${ns}" --for=condition=Ready --timeout=60s \
    || { error "nubus-credentials ExternalSecret did not sync. Check ESO logs."; exit 1; }

    info "Waiting for ESO to sync nubus-sensitive-values (up to 60s)..."
    kubectl wait externalsecret/nubus-sensitive-values \
        -n "${ns}" --for=condition=Ready --timeout=60s \
    || { error "nubus-sensitive-values ExternalSecret did not sync. Check ESO logs."; exit 1; }

    success "  nubus-credentials synced."
    success "  nubus-sensitive-values synced."

    # ── provider-helm Release CR ──────────────────────────────────────────────
    info "Applying nubus Release CR (provider-helm)..."
    kubectl apply -f "${SCRIPT_DIR}/kernel/services/nubus/manifests/dev/release.yaml"

    # If stale NATS was detected and cleared, provider-helm may already report
    # the release as Synced (from a previous reconcile) and will NOT run helm
    # upgrade automatically — leaving the NATS StatefulSet missing. Force a
    # direct helm upgrade in that case to guarantee all resources are present.
    if [[ "$_stale_nats" == "1" ]]; then
        info "Stale NATS was cleared; running helm upgrade to restore missing resources..."
        local _base_vals _dev_vals _sens_vals _reg_cfg
        _base_vals=$(kubectl get configmap nubus-base-values \
            -n "${ns}" -o jsonpath='{.data.values\.yaml}' 2>/dev/null || true)
        _dev_vals=$(kubectl get configmap nubus-dev-values \
            -n "${ns}" -o jsonpath='{.data.values\.yaml}' 2>/dev/null || true)
        _sens_vals=$(kubectl get secret nubus-sensitive-values \
            -n "${ns}" -o jsonpath='{.data.sensitive-values\.yaml}' \
            2>/dev/null | base64 -d 2>/dev/null || true)
        _reg_cfg=$(
            if _u=$(kubectl get secret registry-credentials-helm \
                -n "${CROSSPLANE_NAMESPACE}" -o jsonpath='{.data.username}' 2>/dev/null | base64 -d) && \
                _p=$(kubectl get secret registry-credentials-helm \
                    -n "${CROSSPLANE_NAMESPACE}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d) && \
                _auth=$(printf '%s:%s' "${_u}" "${_p}" | base64 -w0); then
                printf '{"auths":{"registry.opencode.de":{"auth":"%s"}}}' "${_auth}"
            fi
        )
        local _nubus_chart_repo _nubus_chart_ver
        _nubus_chart_repo=$(kubectl get release.helm.crossplane.io "${release_name}" \
            -o jsonpath='{.spec.forProvider.chart.repository}' 2>/dev/null || true)
        _nubus_chart_ver=$(kubectl get release.helm.crossplane.io "${release_name}" \
            -o jsonpath='{.spec.forProvider.chart.version}' 2>/dev/null || true)
        if [[ -n "$_base_vals" && -n "$_sens_vals" && -n "$_nubus_chart_repo" ]]; then
            printf '%s' "$_base_vals"  > /tmp/_nubus_base.yaml
            printf '%s' "$_dev_vals"   > /tmp/_nubus_dev.yaml
            printf '%s' "$_sens_vals"  > /tmp/_nubus_sens.yaml
            printf '%s' "$_reg_cfg"    > /tmp/_nubus_reg.json
            helm upgrade "${release_name}" \
                "${_nubus_chart_repo}/nubus" \
                --version "${_nubus_chart_ver}" \
                -n "${ns}" \
                --reuse-values \
                -f /tmp/_nubus_base.yaml \
                -f /tmp/_nubus_dev.yaml \
                -f /tmp/_nubus_sens.yaml \
                --registry-config /tmp/_nubus_reg.json \
                --timeout 5m 2>&1 | tail -3 || true
            rm -f /tmp/_nubus_base.yaml /tmp/_nubus_dev.yaml \
                  /tmp/_nubus_sens.yaml /tmp/_nubus_reg.json
            success "helm upgrade complete — NATS StatefulSet restored."
        else
            warn "Could not gather helm values for forced upgrade; NATS may need manual recovery."
        fi
    fi

    # Wait for the register-consumers job to appear (provider-helm must reconcile
    # and helm-install the chart first) then wait for it to complete successfully.
    info "Waiting for register-consumers job to appear (up to 5m)..."
    # Match any revision suffix to be resilient to multiple helm upgrade cycles.
    local deadline=$((SECONDS + 300))
    local job_name=""
    until [[ -n "$job_name" ]]; do
        job_name=$(kubectl get jobs -n "${ns}" --no-headers \
            -o custom-columns=NAME:.metadata.name 2>/dev/null \
            | grep "^${release_name}-provisioning-register-consumers-" \
            | tail -1 || true)
        if (( SECONDS > deadline )); then
            warn "  register-consumers job did not appear within 5m — continuing async."
            warn "  Monitor: kubectl get pods -n ${ns} -l app.kubernetes.io/component=register-consumers"
            success "Nubus Release submitted via provider-helm."
            return 0
        fi
        [[ -n "$job_name" ]] || sleep 5
    done
    info "Waiting for register-consumers job to complete (up to 2m)..."
    if kubectl wait "job/${job_name}" -n "${ns}" \
            --for=condition=Complete --timeout=120s 2>/dev/null; then
        success "  Consumer registration complete."
    else
        warn "  register-consumers job did not complete within 2m."
        warn "  Check: kubectl logs -n ${ns} -l job-name=${job_name} --tail=20"
    fi
    success "Nubus deployed via provider-helm."

    # ── Wait for stack-data-ums job; auto-recover if it fails ────────────────
    # The stack-data-ums job:
    #   1. Creates settings/extended_attribute LDAP objects (opendesk properties)
    #   2. Immediately uses those properties to update the Administrator user
    # The UDM REST API caches its module registry at startup, so it doesn't
    # know about extended_attributes created in step 1.  If the job fails with
    # "The User module has no property opendeskFileshare*", restart the UDM
    # REST API (which reloads the module registry from LDAP) then reapply the job.
    # Also handles: if the job fails with "globaladdressbookdisabled has invalid
    # value" (stale opendesk_standard profile from a previous partial install),
    # remove the stale profile from LDAP, restart UDM, and reapply the job.
    _wait_and_fix_stack_data_ums() {
        local sdu_job="" sdu_ns="${ns}" sdu_deadline
        info "Waiting for stack-data-ums job to appear (up to 5m)..."
        sdu_deadline=$((SECONDS + 300))
        until [[ -n "$sdu_job" ]]; do
            sdu_job=$(kubectl get jobs -n "${sdu_ns}" --no-headers \
                -o custom-columns=NAME:.metadata.name 2>/dev/null \
                | grep -E "^${release_name}-stack-data-ums-[0-9]+" | tail -1 || true)
            if (( SECONDS > sdu_deadline )); then
                warn "  stack-data-ums job did not appear in 5m — skipping wait."
                return 0
            fi
            [[ -n "$sdu_job" ]] || sleep 5
        done

        # Patch TTL early so failed retry pods are garbage-collected even if
        # install is interrupted before finalize_stack_data_ums_job runs.
        finalize_stack_data_ums_job "${sdu_ns}" "${sdu_job}"

        info "Waiting for stack-data-ums job '${sdu_job}' to complete (up to 10m)..."
        if kubectl wait "job/${sdu_job}" -n "${sdu_ns}" \
                --for=condition=Complete --timeout=600s 2>/dev/null; then
            success "  stack-data-ums job completed successfully."
            finalize_stack_data_ums_job "${sdu_ns}" "${sdu_job}"
            return 0
        fi

        # Job failed — check if it's the known extended_attribute cache issue.
        local sdu_logs
        sdu_logs=$(kubectl logs -n "${sdu_ns}" \
            -l "app.kubernetes.io/name=stack-data-ums,job-name=${sdu_job}" \
            --tail=30 2>/dev/null || true)
        if printf '%s' "${sdu_logs}" | grep -qE "has no property opendesk|globaladdressbookdisabled"; then
            if printf '%s' "${sdu_logs}" | grep -q "globaladdressbookdisabled"; then
                warn "  stack-data-ums failed: stale opendesk_standard profile (invalid globaladdressbookdisabled)."
                warn "  Removing stale opendesk_standard accessprofile from LDAP..."
                kubectl exec -n "${sdu_ns}" \
                    "${release_name}-ldap-server-primary-0" -- \
                    ldapdelete -Y EXTERNAL -H ldapi:/// \
                    "cn=opendesk_standard,cn=accessprofiles,cn=open-xchange,dc=swp-ldap,dc=internal" \
                    2>/dev/null || true
            else
                warn "  stack-data-ums failed: UDM REST API had stale module cache."
            fi
            warn "  Restarting UDM REST API to reload extended_attribute definitions..."
            kubectl rollout restart deployment "${release_name}-udm-rest-api" \
                -n "${sdu_ns}" 2>/dev/null || true
            kubectl rollout status deployment "${release_name}-udm-rest-api" \
                -n "${sdu_ns}" --timeout=2m 2>/dev/null || true
            success "  UDM REST API restarted."

            # Delete the failed job and reapply from the Helm manifest.
            info "  Reapplying stack-data-ums job..."
            kubectl delete job "${sdu_job}" -n "${sdu_ns}" \
                --ignore-not-found=true 2>/dev/null || true
            apply_stack_data_ums_job_from_helm "${release_name}" "${sdu_ns}" || true

            info "  Waiting for reapplied stack-data-ums job to complete (up to 10m)..."
            sdu_deadline=$((SECONDS + 600))
            local new_job=""
            until [[ -n "$new_job" ]]; do
                new_job=$(kubectl get jobs -n "${sdu_ns}" --no-headers \
                    -o custom-columns=NAME:.metadata.name 2>/dev/null \
                    | grep -E "^${release_name}-stack-data-ums-[0-9]+" | tail -1 || true)
                if (( SECONDS > sdu_deadline )); then
                    warn "  Reapplied stack-data-ums job did not appear — check manually."
                    return 1
                fi
                [[ -n "$new_job" ]] || sleep 3
            done
            if kubectl wait "job/${new_job}" -n "${sdu_ns}" \
                    --for=condition=Complete --timeout=600s 2>/dev/null; then
                success "  stack-data-ums job succeeded after UDM restart."
                finalize_stack_data_ums_job "${sdu_ns}" "${new_job}"
            else
                warn "  stack-data-ums still failing after UDM restart."
                warn "  Check: kubectl logs -n ${sdu_ns} -l job-name=${new_job} --tail=40"
                return 1
            fi
        else
            warn "  stack-data-ums job failed for an unknown reason."
            warn "  Check: kubectl logs -n ${sdu_ns} -l job-name=${sdu_job} --tail=40"
            return 1
        fi
    }
    _wait_and_fix_stack_data_ums || true
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
    echo -e "${GREEN}  Completed      : Steps 14–16 (Stage 1 IdP, authz bridge, Gentian portal)${NC}"
    # Portal credentials (MASTER_PASSWORD-derived; same as keycloak-portal-bootstrap Job).
    if [[ -f "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh" ]]; then
        # shellcheck source=scripts/portal-login-bootstrap.sh
        source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
        print_portal_login_summary
    fi
    echo ""
    echo -e "${GREEN}  OpenDesk apps  : not deployed (Nubus / Intercom / Nextcloud commented out in install.sh)${NC}"
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
    echo -e "${GREEN}  Re-enable OpenDesk app deploy steps in install.sh main_cp() when migrating legacy apps.${NC}"
    echo ""
    echo -e "${GREEN}  Gentian OS infra bootstrap complete.${NC}"
    echo ""
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

    load_deployments_cluster_settings

    [[ "${INSTALL_VALIDATE_ONLY:-0}" == "1" ]] && validate_config

    prompt_app_repos
    prompt_credentials
    prompt_kernel_domain
    prompt_network_mode
    prompt_kernel_secrets
    CROSSPLANE_MODE=1 check_prereqs
    _ensure_bao

    # ── Crossplane core + providers ──────────────────────────────────────────
    install_crossplane          # Step 0   — Crossplane controller
    install_crossplane_providers  # Step 0b/0c — providers, XRD, Composition

    # ── Cluster infrastructure ───────────────────────────────────────────────
    create_namespaces           # Step 1
    prewarm_cluster             # Step 2
    install_cert_manager        # Step 3
    install_kernel_cert_resources  # Step 3b — ClusterIssuers
    install_envoy_gateway       # Step 3c — Envoy Gateway (ROUTING_MODE=gateway)
    install_eso                 # Step 4

    # ── ArgoCD + OpenBao bootstrap ────────────────────────────────────────────
    install_argocd              # Step 5
    setup_argocd_repos          # Step 5b — opencode OCI repo credentials (optional; for future OpenDesk apps)
    install_argocd_image_updater  # Step 5c
    bootstrap_transit_app       # Step 6  — transit seal ArgoCD app
    init_openbao_transit        # Step 7  — transit init + auto-unseal Secret
    bootstrap_argocd_apps       # Step 8  — openbao, reloader, cnpg, globals
    init_openbao                # Step 9  — primary OpenBao init (BAO_TOKEN set here)

    # ── Crossplane kernel provisioning ───────────────────────────────────────
    bootstrap_openbao_for_crossplane  # Step 10 — K8s auth + crossplane-write policy
    create_crossplane_secrets         # Step 11 — derived-credential K8s Secrets
    apply_cluster_xr                  # Step 12 — Cluster XR → all kernel MRs
    seed_secrets_remaining            # Step 12b — remaining paths (registry, DNS, etc.)

    # ── Optional TLS wildcard ─────────────────────────────────────────────────
    install_kernel_wildcard     # Step 12c (optional) — wildcard cert (requires CF_API_TOKEN)
    bootstrap_root_appset       # Step 12d — root app-of-apps (minio, redis, mariadb, IAM…)

    # ── Pattern B chart deployments ─────────────────────────────────────────
    install_provider_helm       # Step 13 — wait for provider-helm Healthy
    apply_infra_data_xr         # Step 13b — shared PostgreSQL + MariaDB via InfraData XR
    install_mac_admission       # Step 13c — Kyverno admission (Stage 0 MAC)

    # ── Stage 1: OpenFGA + standalone Keycloak + authz bridge ───────────────
    apply_suze_xr               # Step 14 — Gentian IdP (Keycloak + OpenFGA) via Suze XR
    install_stage1_operator     # Step 15 — operator with authz bridge
    # shellcheck source=scripts/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
    install_stage1_portal       # Step 16 — portal OIDC login dogfood

    # ── OpenDesk app stack (commented — uncomment when migrating legacy apps) ─
    # deploy_nubus                # Step 16 — Nubus namespaces + ESO Secrets + Release CR
    # "${SCRIPT_DIR}/update.sh" --fix-kernel-ldap-scope  # Step 16b — kernel LDAP SUBTREE
    # deploy_kernel_mail_services # Step 17b — Postfix + Dovecot (MAIL_SERVICE_MODE=kernel)
    # apply_kernel_gateway_overlays || true  # Step 17a — gateway value overlays
    # wait_for_gateway_platform || true    # Step 17d — kernel Gateway + HTTPRoutes when ROUTING_MODE=gateway
    # bootstrap_appprofiles       # Step 17c — AppProfile CRs from gentian-apps repo
    #
    # wait_for_setup_iam_job || true
    # verify_argocd_apps || true
    # verify_keycloak_iframe_policy || true
    # verify_intercom_ics || true
    # reconcile_nextcloud_office || true  # Step 18c — Collabora / Nextcloud office
    # configure_github_actions_secrets   # Step 18d — CI_BOT_PAT → gentian-os Actions secrets

    success "Bootstrap complete — Stage 1 IdP (Keycloak + OpenFGA), authz bridge, and portal login are live."
    unset INSTALL_START_EPOCH
    save_install_state
    print_summary_cp
}

run_stage1_portal_only() {
    load_creds_cache
    load_install_state
    try_load_creds_from_openbao
    load_deployments_cluster_settings
    prompt_kernel_domain 2>/dev/null || true
    [[ -n "${KERNEL_DOMAIN:-}" ]] || { error "KERNEL_DOMAIN not set — source install.env or run full install first."; exit 1; }
    [[ -n "${MASTER_PASSWORD:-}" ]] || { error "MASTER_PASSWORD not set — source install.env."; exit 1; }
    # shellcheck source=scripts/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
    install_stage1_portal
}

case "${1:-}" in
    --stage1-portal)
        run_stage1_portal_only
        exit 0
        ;;
esac

main_cp "$@"
