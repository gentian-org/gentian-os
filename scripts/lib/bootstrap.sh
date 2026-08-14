#!/usr/bin/env bash
# =============================================================================
# scripts/lib/bootstrap.sh — bootstrap step bodies
# =============================================================================
# The bodies of the install steps. They lived in install.sh until install.sh became a driver;
# moving them here rather than into each step file keeps Phase 0a a pure restructure, and lets
# Phase 4b delete the ones that go declarative without touching the steps that stay.
# =============================================================================

[[ -n "${GENTIAN_BOOTSTRAP_LOADED:-}" ]] && return 0
GENTIAN_BOOTSTRAP_LOADED=1

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

    # Grant provider-kubernetes/provider-helm rights over the objects their
    # Compositions manage. Applied before the Healthy wait so the permissions
    # exist by the time the first Object is reconciled — a ClusterRoleBinding may
    # reference a ServiceAccount that does not exist yet.
    info "Applying provider RBAC (InjectedIdentity needs an explicit grant)..."
    _kubectl_retry apply -f "${SCRIPT_DIR}/crossplane/providers/provider-rbac.yaml"

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
    # Repositories: one claim per Git repo or OCI registry the cluster draws
    # from, emitting its own CredentialRequirement alongside the ArgoCD repo
    # Secret, the AppProject whitelist entry and (for oci) the pull secret and
    # ImageConfig. Adding a repository is one claim and no new Composition.
    _kubectl_retry apply -f "${SCRIPT_DIR}/crossplane/xrds/repository.yaml"
    _adopt_xrd_crds xtenants.gentianos.io xtenants.gentianos.io tenants.gentianos.io
    _kubectl_retry wait xrd xtenants.gentianos.io \
        --for=condition=Established --timeout=2m

    success "Crossplane providers, XRDs, and Compositions are ready."
}

# =============================================================================
bootstrap_openbao_for_crossplane() {
    banner "Step 8 — Bootstrap OpenBao auth for Crossplane"

    if ! VAULT_ADDR=$(gentian_service_addr openbao openbao 8200 https); then
        error "Could not reach the openbao Service on :8200."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        exit 1
    fi
    export VAULT_ADDR
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
    # ifk-l2-prod-k4d2m). Read it from the Claim's resourceRef once populated.
    local claim_name
    claim_name="$(gentian_cluster_claim_name)"
    info "Waiting for Claim ${claim_name} to be bound to a composite (up to 60s)..."
    local xr_name=""
    local deadline=$((SECONDS + 60))
    until [[ -n "${xr_name}" ]]; do
        xr_name=$(kubectl get cluster.gentianos.io "${claim_name}" -n crossplane-system \
            -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
        if (( SECONDS > deadline )); then
            error "Claim ${claim_name} was never bound to a composite after 60s."
            error "  kubectl describe cluster.gentianos.io ${claim_name} -n crossplane-system"
            exit 1
        fi
        [[ -n "${xr_name}" ]] || sleep 3
    done
    info "  Composite name: ${xr_name}"

    info "Waiting for XCluster ${xr_name} to be Ready (timeout: ${CLUSTER_XR_TIMEOUT})..."
    kubectl wait "xcluster.gentianos.io/${xr_name}" \
        --for=condition=Ready --timeout="${CLUSTER_XR_TIMEOUT}" \
    || {
        error "XCluster ${xr_name} did not become Ready within ${CLUSTER_XR_TIMEOUT}."
        error "Diagnose with:"
        error "  kubectl describe xcluster.gentianos.io ${xr_name}"
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
    resolve_gentian_os_branch
    # Provenance and the deployments pointer reach the child ApplicationSets
    # through here. Defaults keep an unset install working against the public
    # origin; a mirrored install sets them in install.env (§2, surface 1).
    export GENTIAN_OS_REPO="${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}"
    export GENTIAN_DEPLOYMENTS_REPO="${GENTIAN_DEPLOYMENTS_REPO:-}"
    export GENTIAN_DEPLOYMENTS_BRANCH="${GENTIAN_DEPLOYMENTS_BRANCH:-main}"
    export GENTIAN_DEPLOYMENTS_CLUSTER="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
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


# =============================================================================
# Print Crossplane-aware installation summary
# =============================================================================
print_summary_cp() {
    local xr_name xr_ready mr_count infra_pg_ready infra_mdb_ready infra_redis_ready infra_minio_ready argocd_url argocd_pw

    local claim_name
    claim_name="$(gentian_cluster_claim_name)"
    xr_name=$(kubectl get cluster.gentianos.io "${claim_name}" -n crossplane-system \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
    xr_name="${xr_name:-${claim_name}}"

    xr_ready=$(kubectl get "xcluster.gentianos.io/${xr_name}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    mr_count=$(kubectl get managed -l "crossplane.io/composite=${xr_name}" \
        --no-headers 2>/dev/null | wc -l | tr -d ' ')
    # Releases are named <composite>-<chart>, and the composite carries
    # Crossplane's random suffix (e.g. ifk-l2-prod-infra-data-n6z4s-postgresql).
    # Looking them up under the *claim* name never matched, so these flags always
    # read "unknown" regardless of the actual state. Resolve the composite first.
    local infra_claim infra_xr
    infra_claim="$(gentian_infradata_claim_name)"
    infra_xr=$(kubectl get infradata.gentianos.io "${infra_claim}" -n crossplane-system \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
    infra_xr="${infra_xr:-${infra_claim}}"
    _release_ready() {
        kubectl get "release.helm.crossplane.io/${infra_xr}-$1" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown"
    }
    infra_pg_ready=$(_release_ready postgresql)
    infra_mdb_ready=$(_release_ready mariadb)
    infra_redis_ready=$(_release_ready redis)
    infra_minio_ready=$(_release_ready minio)
    local suze_ready openfga_ready keycloak_ready suze_xr
    suze_ready=$(kubectl get xsuze -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    suze_xr=$(kubectl get suze.gentianos.io "$(gentian_suze_claim_name)" -n crossplane-system \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || gentian_suze_claim_name)
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
    echo -e "${GREEN}  InfraData PG   : ${infra_xr}-postgresql (Ready=${infra_pg_ready})${NC}"
    echo -e "${GREEN}  InfraData MDB  : ${infra_xr}-mariadb (Ready=${infra_mdb_ready})${NC}"
    echo -e "${GREEN}  InfraData Redis: ${infra_xr}-redis (Ready=${infra_redis_ready})${NC}"
    echo -e "${GREEN}  InfraData MinIO: ${infra_xr}-minio (Ready=${infra_minio_ready})${NC}"
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
    echo -e "${GREEN}    kubectl get release.helm.crossplane.io | grep ${infra_xr}${NC}"
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
        info "Deploying GPU vLLM inference backend(s)..."
        render_and_apply_vllm_gpu_manifest "${manifests_dir}"
    else
        info "Deploying mock inference backend (GPU_ACCELERATION=false) — see vllm-gpu.yaml.tmpl to serve a real model."
        kubectl apply -f "${manifests_dir}/vllm-mock.yaml"
        # Mock uses a fixed name that never collides with vllm-<id>-inference,
        # so flipping GPU_ACCELERATION back to false wouldn't otherwise clean
        # up any real instances left running from before.
        prune_stale_vllm_instances ""
    fi

    info "Waiting for llm-sensitive-values ExternalSecret to sync (up to 60s)..."
    kubectl wait externalsecret/llm-sensitive-values \
        -n "${ns}" --for=condition=Ready --timeout=60s \
    || warn "llm-sensitive-values not yet Ready — it will sync when OpenBao is available."

    ensure_litellm_teams || warn "LiteLLM team sync failed — retry with ./update.sh --llm."
    ensure_litellm_vllm_model || warn "LiteLLM vLLM model sync failed — retry with ./update.sh --llm."

    success "LLM serving stack deployment complete."
}
