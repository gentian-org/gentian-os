#!/usr/bin/env bash
# =============================================================================
# install-cp.sh — Gentian OS bootstrap using Crossplane (Phase 1+)
# =============================================================================
# Replaces the OpenTofu-based kernel provisioning with a Crossplane Cluster XR.
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
#   ✓ Remaining secrets seeded (registry, DNS/Cloudflare, internal)
#
# Not done (future phases):
#   Phase 2 — Pattern B app charts via provider-helm
#   Phase 3 — Tenant XRD provisioning
#
# Usage:
#   ./install-cp.sh
#   ./install-cp.sh --validate          # validate config only, no cluster changes
#   ./install-cp.sh --no-cluster-infra  # skip cert-manager/CNPG/reloader
#
# Required environment variables: same as install.sh (see getting-started.sh)
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load all helper functions from install.sh without running its main().
export GENTIAN_INSTALL_LIB_ONLY=1
# shellcheck source=install.sh
source "${SCRIPT_DIR}/install.sh"
unset GENTIAN_INSTALL_LIB_ONLY

# ─── Crossplane settings ──────────────────────────────────────────────────────
CROSSPLANE_NAMESPACE=crossplane-system
CROSSPLANE_VERSION="1.18.0"
CROSSPLANE_HELM_REPO=https://charts.crossplane.io/stable
PROVIDER_WAIT_TIMEOUT=15m
CLUSTER_XR_TIMEOUT=15m

# =============================================================================
# Crossplane 0 — Install Crossplane core
# (mirrors the logic of crossplane/tests/e2e/scripts/p0-crossplane-install.sh)
# =============================================================================
install_crossplane() {
    banner "Crossplane 0 — Install Crossplane core"

    if kubectl get deployment crossplane -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1; then
        success "Crossplane deployment already present in ${CROSSPLANE_NAMESPACE}; skipping."
        return
    fi
    if helm status crossplane -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1; then
        success "Crossplane already installed via Helm; skipping."
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
    success "Crossplane core installed and Ready."
}

# =============================================================================
# Crossplane 0b/0c — Install providers, XRD, Composition
# (mirrors crossplane/tests/e2e/scripts/p1-kernel-dev.sh steps 1-3)
# =============================================================================
install_crossplane_providers() {
    banner "Crossplane 0b/0c — Providers, XRD, Composition"

    info "Applying providers (function-go-templating, provider-kubernetes, provider-vault)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/providers/providers.yaml"

    info "Waiting for providers to become Healthy (timeout: ${PROVIDER_WAIT_TIMEOUT})..."

    # function-go-templating is a Function resource; provider-kubernetes and
    # provider-vault are Provider resources. Use the correct type for each so
    # we don't burn the full timeout on the wrong resource kind.
    info "  Waiting for: function-go-templating"
    kubectl wait function.pkg.crossplane.io/function-go-templating \
        --for=condition=Healthy --timeout="${PROVIDER_WAIT_TIMEOUT}"

    for provider in provider-kubernetes provider-vault; do
        info "  Waiting for: ${provider}"
        kubectl wait "provider.pkg.crossplane.io/${provider}" \
            --for=condition=Healthy --timeout="${PROVIDER_WAIT_TIMEOUT}"
    done

    # Apply ProviderConfigs only after all providers are Healthy so the CRDs
    # (e.g. vault.upbound.io/v1beta1 ProviderConfig) exist.
    info "Applying ProviderConfigs (InjectedIdentity for both kubernetes and openbao)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/providers/provider-configs.yaml"

    info "Applying XRD (XCluster / Cluster)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/xrds/cluster.yaml"
    kubectl wait xrd xclusters.gentianos.io \
        --for=condition=Established --timeout=2m

    info "Applying Composition (cluster-default)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/cluster-default.yaml"

    success "Crossplane providers, XRD, and Composition are ready."
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
    banner "Step 10 — Bootstrap OpenBao auth for Crossplane (replaces tofu apply openbao-init)"

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

    # ── 1. KV v2 mount at 'secret/' ──────────────────────────────────────────
    if bao secrets list -format=json 2>/dev/null | jq -e '."secret/"' >/dev/null 2>&1; then
        success "KV v2 mount at 'secret/' already present."
    else
        bao secrets enable -path=secret kv-v2
        success "KV v2 mount at 'secret/' enabled."
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
    bao policy write crossplane-write - <<'POLICY'
# KV operations
path "secret/data/gentian-os/*"     { capabilities = ["create","read","update","delete"] }
path "secret/metadata/gentian-os/*" { capabilities = ["list","read","delete"] }
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

    # ── 4. crossplane-provider Kubernetes auth role ───────────────────────────
    # Kept for future use (K8s auth for dynamic tokens); not used by the
    # provider-vault ProviderConfig which requires a static token Secret.
    bao write auth/kubernetes/role/crossplane-provider \
        bound_service_account_names=crossplane-provider-vault \
        bound_service_account_namespaces="${CROSSPLANE_NAMESPACE}" \
        token_policies=crossplane-write \
        token_ttl=3600
    success "crossplane-provider K8s auth role created."

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
    _derive() { echo -n "${1}:${2}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" -binary | sha1sum | awk '{print $1}'; }
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
            --arg h "$(_derive postgres nextcloud_user)" \
            '{postgres_password:$a,keycloak_user_password:$b,keycloak_extensions_user_password:$c,selfservice_user_password:$d,authsession_user_password:$e,guardianmanagementapi_user_password:$f,notificationsapi_user_password:$g,nextcloud_user_password:$h}')"

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
            --arg d "$(_derive minio nextcloud_user)" \
            --arg e "$(_derive minio openxchange_user)" \
            --arg f "$(_derive minio openproject_user)" \
            --arg g "$(_derive minio notes_user)" \
            --arg h "$(_derive minio migrations_user)" \
            --arg i "$(_derive minio dovecot_user)" \
            '{root_user:$a,root_password:$b,ums_password:$c,nextcloud_password:$d,openxchange_password:$e,openproject_password:$f,notes_password:$g,migrations_password:$h,dovecot_password:$i}')"

    # ── identity/nubus ────────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-identity-nubus" \
        "$(jq -nc \
            --arg mp "${MASTER_PASSWORD}" \
            --arg a  "$(_derive nubus Administrator)" \
            --arg b  "$(_derive "cn=admin" ldap)" \
            --arg c  "$(_derive keycloak adminPassword)" \
            --arg d  "$(_derive nubus ox_system_user)" \
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
            --arg r  "$(_derive nubus ldapsearch_nextcloud)" \
            --arg s  "$(_derive nubus ldapsearch_dovecot)" \
            --arg t  "$(_derive nubus ldapsearch_element)" \
            --arg u  "$(_derive nubus ldapsearch_ox)" \
            --arg v  "$(_derive nubus ldapsearch_postfix)" \
            --arg w  "$(_derive nubus ldapsearch_openproject)" \
            --arg x  "$(_derive nubus ldapsearch_xwiki)" \
            --arg y  "$(_derive centralnavigation api_key)" \
            --arg z  "$(_derive portal-consumer provisioning-api)" \
            --arg z2 "$(_derive selfservice-consumer provisioning-api)" \
            '{master_password:$mp,admin_password:$a,ldap_admin_password:$b,keycloak_admin_password:$c,ox_system_user_password:$d,nats_api_password:$e,nats_dispatcher_password:$f,nats_prefill_password:$g,nats_udm_listener_password:$h,nats_udm_transformer_password:$i,minio_ums_secret_access_key:$j,pg_selfservice_password:$k,pg_authsession_password:$l,pg_keycloak_password:$m,pg_keycloak_extensions_password:$n,pg_guardian_password:$o,pg_notifications_password:$p,ldapsearch_keycloak:$q,ldapsearch_nextcloud:$r,ldapsearch_dovecot:$s,ldapsearch_element:$t,ldapsearch_ox:$u,ldapsearch_postfix:$v,ldapsearch_openproject:$w,ldapsearch_xwiki:$x,portal_shared_secret:$y,portal_consumer_api_password:$z,selfservice_consumer_api_password:$z2}')"

    # ── identity/keycloak-bootstrap ───────────────────────────────────────────
    _kv_secret "gentian-os-kernel-identity-keycloak-bootstrap" \
        "$(jq -nc \
            --arg a "$(_derive keycloak adminPassword)" \
            --arg b "$(_derive keycloak intercom_client_secret)" \
            '{admin_password:$a,intercom_client_secret:$b}')"

    # ── mail/postfix (HMAC-derived fields + operator-supplied relay credentials) ─
    _kv_secret "gentian-os-kernel-mail-postfix" \
        "$(jq -nc \
            --arg host "${EXTERNAL_SMTP_HOST:-}" \
            --arg port "${EXTERNAL_SMTP_PORT:-587}" \
            --arg user "${OD_SMTP_RELAY_USERNAME:-}" \
            --arg pass "${OD_SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port,relay_username:$user,relay_password:$pass}')"

    success "All 8 input Secrets applied to ${CROSSPLANE_NAMESPACE}."
}

# =============================================================================
# Crossplane step 12 — Apply Cluster claim and wait for Ready.
# The Cluster XR creates all 19 kernel MRs via provider-vault and
# provider-kubernetes. managementPolicies: [Observe,Create] on KV seeds
# ensures existing paths seeded by prior install runs are never overwritten.
# =============================================================================
apply_cluster_xr() {
    banner "Step 12 — Apply Cluster XR (kernel structural provisioning)"

    info "Applying crossplane/claims/dev-cluster.yaml..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/claims/dev-cluster.yaml"

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
# Phase 2 — Step 13: Install provider-helm
# provider-helm deploys Helm charts into the local cluster. It replaces the
# Tofu Controller set_sensitive pattern for secrets-hostile charts (Pattern B).
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
# Phase 2 — Step 14: Deploy Nubus via provider-helm (Pattern B migration)
#
# Creates:
#   - gentian-dev + gentian-infra-dev namespaces
#   - registry-credentials-helm Secret (crossplane-system) for OCI chart pull
#   - registry-credentials imagePullSecret (gentian-dev) for pod image pull
#   - nubus-base-values + nubus-dev-values ConfigMaps (non-sensitive values)
#   - nubus-dev-udm-listener-nats-patch ConfigMap (NATS subject bug workaround)
#   - ExternalSecrets: nubus-credentials + nubus-sensitive-values (via ESO)
#   - provider-helm Release CR (nubus-dev)
# =============================================================================
deploy_nubus() {
    banner "Step 14 — Deploy Nubus via provider-helm"

    local ns="gentian-${ENV:-dev}"
    local infra_ns="gentian-infra-${ENV:-dev}"
    local release_name="nubus-${ENV:-dev}"

    # ── Namespaces ────────────────────────────────────────────────────────────
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
    info "Creating nubus values ConfigMaps in ${ns}..."
    kubectl create configmap nubus-base-values \
        -n "${ns}" \
        --from-file=values.yaml="${SCRIPT_DIR}/crossplane/apps/nubus/values/_base.yaml" \
        --dry-run=client -o yaml | kubectl apply -f -
    kubectl create configmap nubus-dev-values \
        -n "${ns}" \
        --from-file=values.yaml="${SCRIPT_DIR}/crossplane/apps/nubus/values/dev.yaml" \
        --dry-run=client -o yaml | kubectl apply -f -

    # ── NATS subject patch ConfigMap ──────────────────────────────────────────
    # Fixes LDAP_SUBJECT mismatch between udm-listener and udm-transformer
    # images in nubus 1.16.0. Referenced by nubusUdmListener.extraVolumes.
    info "Creating ${release_name}-udm-listener-nats-patch ConfigMap in ${ns}..."
    kubectl create configmap "${release_name}-udm-listener-nats-patch" \
        -n "${ns}" \
        --from-file=mq_adapter_nats.py="${SCRIPT_DIR}/crossplane/apps/nubus/patches/mq_adapter_nats.py" \
        --dry-run=client -o yaml | kubectl apply -f -

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
    kubectl apply -f "${SCRIPT_DIR}/crossplane/apps/nubus/release.yaml"

    info "Nubus Release submitted. Chart deployment proceeds asynchronously."
    info "  Monitor: kubectl get release.helm.crossplane.io/nubus-dev"
    info "  Pods:    kubectl get pods -n ${ns}"
    success "Phase 2 — Nubus Release submitted via provider-helm."
}

# =============================================================================
# Print Crossplane-aware installation summary
# =============================================================================
print_summary_cp() {
    local xr_name
    xr_name=$(kubectl get cluster dev-cluster -n crossplane-system \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
    xr_name="${xr_name:-dev-cluster}"   # fallback for display if claim missing

    local xr_ready
    xr_ready=$(kubectl get "xcluster/${xr_name}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    local mr_count
    mr_count=$(kubectl get managed -l "crossplane.io/composite=${xr_name}" \
        --no-headers 2>/dev/null | wc -l | tr -d ' ')

    local nubus_synced
    nubus_synced=$(kubectl get release.helm.crossplane.io/nubus-dev \
        -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}' 2>/dev/null || echo "unknown")

    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     Gentian OS — Phase 1 + 2 Bootstrap Complete          ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo "  Kernel domain  : ${KERNEL_DOMAIN:-<not set>}"
    echo "  Cluster XR     : ${xr_name} (Ready=${xr_ready}, MRs=${mr_count})"
    echo "  Nubus Release  : nubus-dev (Synced=${nubus_synced})"
    echo ""
    echo "  Inspect Crossplane managed resources:"
    echo "    kubectl get managed -l crossplane.io/composite=${xr_name}"
    echo "    kubectl get release.helm.crossplane.io/nubus-dev"
    echo ""
    echo "  Nubus pods:"
    echo "    kubectl get pods -n gentian-${ENV:-dev} -l app.kubernetes.io/part-of=nubus"
    echo ""
    echo "  ArgoCD:"
    echo "    URL  : https://argocd.${KERNEL_DOMAIN:-<kernel-domain>}"
    echo "    User : admin"
    echo "    Pass : kubectl get secret argocd-initial-admin-secret -n argocd \\"
    echo "             -o jsonpath='{.data.password}' | base64 -d"
    echo ""
    echo "  OpenBao tokens saved to: ${OPENBAO_INIT_FILE}"
    echo ""
    echo "  Next: Phase 3 (Tenant XRD shadow deployment)"
    echo "        see docs/crossplane-migration-plan.md"
    echo ""
}

# =============================================================================
# main — Crossplane-based bootstrap (Phase 1)
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

    [[ "${INSTALL_VALIDATE_ONLY:-0}" == "1" ]] && validate_config

    prompt_credentials
    prompt_kernel_domain
    prompt_network_mode
    prompt_kernel_secrets
    CROSSPLANE_MODE=1 check_prereqs

    # ── Crossplane core + providers ──────────────────────────────────────────
    install_crossplane          # Step 0   — Crossplane controller
    install_crossplane_providers  # Step 0b/0c — providers, XRD, Composition

    # ── Cluster infrastructure ───────────────────────────────────────────────
    create_namespaces           # Step 1
    prewarm_cluster             # Step 2
    install_cert_manager        # Step 3
    install_kernel_cert_resources  # Step 3b — ClusterIssuers
    install_eso                 # Step 4

    # ── ArgoCD + OpenBao bootstrap ────────────────────────────────────────────
    install_argocd              # Step 5
    setup_argocd_repos          # Step 5b
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
    install_kernel_wildcard     # Step 13b — wildcard cert (requires CF_API_TOKEN)

    # ── Phase 2: Pattern B chart deployments ─────────────────────────────────
    install_provider_helm       # Step 13 — provider-helm provider
    deploy_nubus                # Step 14 — Nubus via provider-helm

    print_summary_cp
}

main_cp "$@"
