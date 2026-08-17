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

install_crossplane() {
    banner "Install Crossplane core"

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
        _delete_namespace "${CROSSPLANE_NAMESPACE}"
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
# _unstick_terminating_xrds — clear XRDs wedged mid-deletion.
#
# An XRD stuck Terminating cannot be re-created, and its claim CRD stays
# absent, so every step that applies one of those claims fails with "no
# matches for kind" and no indication that the cause is a deletion from
# hours earlier that never finished. Composites hold a finalizer their
# controller clears — but when the XRD is already half-gone the controller
# no longer reconciles them, so the two wait on each other indefinitely.
_unstick_terminating_xrds() {
    local xrd claim_kind leftover
    for xrd in $(kubectl get xrd -o jsonpath='{range .items[?(@.metadata.deletionTimestamp)]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
        warn "XRD ${xrd} is stuck Terminating; clearing what blocks it."
        claim_kind="$(kubectl get xrd "${xrd}" -o jsonpath='{.spec.names.plural}' 2>/dev/null)"
        [[ -n "${claim_kind}" ]] || continue
        for leftover in $(kubectl get "${claim_kind}.gentianos.io" -A \
            -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
            kubectl patch "${claim_kind}.gentianos.io" "${leftover}" --type=merge \
                -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
        done
        kubectl wait xrd "${xrd}" --for=delete --timeout=60s >/dev/null 2>&1 || true
    done
}

install_crossplane_providers() {
    banner "Crossplane providers, XRD, Composition"

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
    _unstick_terminating_xrds

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

    # Everything else in the directory, so the step's `provides: XRDs` is the
    # whole set rather than the four named above. infra-data and suze were only
    # ever applied by the crossplane-xrds ArgoCD Application, which means a
    # cluster whose Application had not synced yet — or had been pruned — was
    # missing the XRDs that compose Keycloak, OpenFGA and the infra databases,
    # with nothing in the installer able to put them back.
    local xrd_file
    for xrd_file in "${SCRIPT_DIR}"/crossplane/xrds/*.yaml; do
        [[ -f "${xrd_file}" ]] || continue
        case "${xrd_file##*/}" in
            cluster.yaml|app.yaml|tenant.yaml|repository.yaml) continue ;;
        esac
        info "Applying XRD ($(basename "${xrd_file}" .yaml))..."
        _kubectl_retry apply -f "${xrd_file}"
    done

    # Established, not merely applied: the claims that follow are rejected by an
    # XRD whose CRD the API server has not finished registering.
    local xrd_name
    for xrd_name in $(kubectl get xrd -o name 2>/dev/null); do
        kubectl wait "${xrd_name}" --for=condition=Established --timeout=2m >/dev/null 2>&1 || true
    done

    success "Crossplane providers, XRDs, and Compositions are ready."
}

# =============================================================================
# =============================================================================
# bootstrap_openbao_for_crossplane — the minimum that must precede Crossplane
# =============================================================================
# Everything this writes is something Crossplane needs in order to manage
# OpenBao at all, and therefore something Crossplane must NOT manage:
#
#   the KV mount      — where its SecretV2 resources write
#   crossplane-write  — the policy its token carries
#   the token Secret  — how provider-vault authenticates
#
# A composition that managed its own authorisation could lock itself out: drift
# or a delete on any of the three leaves provider-vault unable to reconcile the
# resource that would restore it.
#
# Everything else — the Kubernetes auth backend and its config, the eso-read
# policy, both auth roles, and the OIDC write path — is declared by the Cluster
# composition and is deliberately absent here.
# =============================================================================
bootstrap_openbao_for_crossplane() {
    banner "OpenBao bootstrap for Crossplane (mount, policy, token)"

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
    # The Kubernetes auth backend, its config, the eso-read policy and both auth
    # roles are NOT written here. The Cluster composition declares all of them,
    # and doing it twice puts two writers on one OpenBao object with no way to
    # see them disagree.
    #
    # None of them is needed to reach the composition: provider-vault
    # authenticates with the static token Secret created below
    # (credentials.source: Secret, provider-configs.yaml), not through the
    # Kubernetes backend. Only ESO needs that, and ESO's ClusterSecretStore is
    # itself composed.

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
# _derive <context> <purpose> — the derived-credential function, at file scope.
#
# Same derivation as seed-openbao.sh and
# crossplane/functions/derive-secrets/derive.py.
#
# File scope because it has callers outside the step that first needed it.
# Nested inside create_crossplane_secrets it existed only while B-06 ran, and
# `declare -F _derive` — which _keycloak_smtp_settings tests before deriving the
# Postfix password — was therefore false everywhere else. On every
# MAIL_SERVICE_MODE=kernel cluster that test failed, so Keycloak realm SMTP was
# skipped with "SMTP credentials incomplete" and the realm could not send an
# invitation or a password reset, while the credentials it needed existed.
# =============================================================================
_derive() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        openssl rand -hex 32
    else
        echo -n "${1}:${2}" | openssl dgst -sha256 \
            -hmac "${MASTER_PASSWORD}${DERIVATION_SALT}" | awk '{print $2}'
    fi
}

# =============================================================================
# Crossplane step 11 — Create derived-credential Secrets in crossplane-system.
#
# The Cluster XR's SecretV2 KV-seed MRs reference these K8s Secrets via
# dataJsonSecretRef. Uses the same HMAC-SHA256 derivation as seed-openbao.sh.
# --dry-run=client | kubectl apply ensures idempotency.
# =============================================================================
create_crossplane_secrets() {
    banner "Create derived-credential Secrets for Cluster XR"

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
    banner "Apply Cluster XR (kernel structural provisioning)"

    local claims_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims"
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
    # seed_secrets() is defined in scripts/lib/openbao.sh (sources
    # seed-openbao.sh). The Cluster XR already wrote the HMAC-derived paths (or
    # observed them if they pre-existed). seed_secrets skips those via
    # kv_put_once and writes only the paths the Cluster XR does not cover.
    seed_secrets
    seed_repository_credentials
}

# =============================================================================
# seed_repository_credentials — persist the tier-0 deployments token
# =============================================================================
# The installer collects this token at the credential prompt, and until now the
# only thing it did with it was create a Secret imperatively. Nothing wrote it
# to OpenBao, so the Repository claim in step 16b would gate on a path that
# never gets a value: its ExternalSecrets would never sync, the ArgoCD
# repository credential would never appear, and the gentian-claims
# ApplicationSet would never be able to read the deployments repo.
#
# The path sits under gentian-os/kernel/ because the eso-read policy grants read
# on that prefix and nothing else — a credential stored anywhere else is
# unreadable by ESO no matter who wrote it.
# =============================================================================
seed_repository_credentials() {
    if [[ -z "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" ]]; then
        info "No deployments repository token supplied; skipping its OpenBao path."
        return 0
    fi
    local username="${GENTIAN_DEPLOYMENTS_GIT_USERNAME:-x-access-token}"
    info "Seeding gentian-os/kernel/repositories/deployments..."
    # kv put, not kv_put_once: a rotated token must actually replace the old one.
    if bao kv put -mount=secret "gentian-os/kernel/repositories/deployments" \
        "username=${username}" \
        "password=${GENTIAN_DEPLOYMENTS_GIT_TOKEN}" >/dev/null 2>&1; then
        success "Deployments repository credential stored."
    else
        error "Could not write the deployments repository credential to OpenBao."
        error "  The Repository claim in step 16b will not become satisfied without it."
        return 1
    fi
}

# =============================================================================
# Apply root ArgoCD ApplicationSet
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
    banner "Bootstrap root ArgoCD ApplicationSet (app-of-apps)"

    export GENTIAN_DEPLOYMENTS_STAGE="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"
    resolve_gentian_os_branch
    # Provenance and the deployments pointer reach the child ApplicationSets
    # through here. Defaults keep an unset install working against the public
    # origin; a mirrored install sets them in install.env (§2, surface 1).
    export GENTIAN_OS_REPO="${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}"
    export GENTIAN_DEPLOYMENTS_REPO="${GENTIAN_DEPLOYMENTS_REPO:-}"
    export GENTIAN_DEPLOYMENTS_BRANCH="${GENTIAN_DEPLOYMENTS_BRANCH:-main}"
    export GENTIAN_DEPLOYMENTS_CLUSTER_ID="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-default-cluster}"
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
# Bootstrap gentian-catalogue ApplicationSet (profile bundles from gentian-apps)
# =============================================================================
bootstrap_appprofiles() {
    install_catalogue_sync
}

# =============================================================================
# Install provider-helm
# provider-helm deploys Helm charts as Crossplane Managed Resources (InfraData XR,
# kernel services, tenant apps via compositions).
# =============================================================================
install_provider_helm() {
    banner "Install provider-helm"

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
# Apply InfraData XR (shared PostgreSQL, MariaDB, Redis, MinIO)
#
# Provisions kernel data stores via Crossplane InfraData XR.
#
# Prerequisites:
#   - provider-helm Healthy (C-03)
#   - gentian-infra-data AppSet synced ESO Secrets + values ConfigMaps (wave 8)
#   - charts/infra/packages published on GitHub for the target git branch
#     (run ./scripts/tools/publish-infra-charts.sh and push before install when adding charts)
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
        error "  Publish charts with: ./scripts/tools/publish-infra-charts.sh && git push"
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
# Kyverno admission controller (Stage 0 MAC)
#
# Deployed by Argo CD via kernel/appsets/05-admission.yaml (sync wave 5–6).
# This step waits for the controller so later workloads are admitted under policy.
# =============================================================================
install_mac_admission() {
    banner "Kyverno admission controller (Stage 0 MAC)"

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

    # Around a dozen cluster queries, and on a remote API server they add up to
    # the better part of a minute. Announce it: the last thing printed before
    # this was "Bootstrap complete", so silence here reads as a hang at exactly
    # the moment the operator is waiting for their prompt back.
    info "Collecting cluster status for the summary (a dozen queries; this takes a moment)..."

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
    # `|| true` on both: grep exits 1 when the release is absent, which is the
    # normal state until phase D deploys Suze. Under pipefail and the ERR trap
    # that ends the run — the summary, whose whole job is to report state, would
    # abort the install for finding a component not deployed yet.
    local openfga_rel keycloak_rel
    openfga_rel=$(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${suze_xr}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep openfga | head -1 || true)
    keycloak_rel=$(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${suze_xr}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep keycloak | head -1 || true)
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
    echo -e "${GREEN}  Completed      : phases A–E (control-plane, secrets, platform, applications, handover)${NC}"
    # Portal credentials (MASTER_PASSWORD-derived; same as keycloak-portal-bootstrap Job).
    if [[ -f "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh" ]]; then
        # shellcheck source=scripts/lib/portal-login-bootstrap.sh
        source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
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

    banner "Deploying LLM serving stack"
    local env="${ENV:-dev}"
    local ns="platform-kernel"

    # litellm-services.yaml references the litellm-dashboard-sso Secret
    # (GENERIC_CLIENT_ID/SECRET) — ensure it exists even though Step 14
    # (portal bootstrap) runs after this step.
    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
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

# =============================================================================
# scaffold_cluster_deployment — write this cluster's kernel/ directory in
# gentian-deployments: claims/{cluster,infra-data,suze}.yaml, values.yaml and
# cluster-settings.env, generated from KERNEL_DOMAIN and
# GENTIAN_DEPLOYMENTS_STAGE. Reached through `install.sh --prepare-deployment`.
#
# Per-file checks, not a directory-level one: an existing file is never
# overwritten, so re-running converges a partially-written directory and
# preserves every hand edit made to one that is already complete.
#
# Writing is all it does. The files are left in the working tree for the
# operator to read, edit and commit, because this directory is what the cluster
# is — a generated claim pushed unread is a cluster configured by whoever ran
# the installer rather than by anyone who reviewed it, and the repository is
# shared with every other cluster.
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
    # The files are only useful inside the checkout they get committed from.
    # Writing them into a bare directory produces a tree nothing tracks, which
    # looks like success and installs nothing.
    if [[ ! -d "${GENTIAN_DEPLOYMENTS_PATH}/.git" ]]; then
        error "${GENTIAN_DEPLOYMENTS_PATH} is not a git checkout of gentian-deployments."
        error "  Clone it there first:"
        error "    git clone ${GENTIAN_DEPLOYMENTS_REPO:-<deployments-repo>} ${GENTIAN_DEPLOYMENTS_PATH}"
        error "  Or point GENTIAN_DEPLOYMENTS_PATH at an existing checkout."
        return 1
    fi

    local kernel_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel"
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID}"
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
        # The claim is named GENTIAN_DEPLOYMENTS_CLUSTER_ID + _STAGE. This file
        # is written only when absent, and gentian_cluster_claim_name() reads
        # the name back from it, so a cluster keeps whatever name it was
        # scaffolded with.
        cat > "${kernel_dir}/claims/cluster.yaml" <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: ${cluster}-${stage}
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
  name: ${cluster}-${stage}-infra-data
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
  name: ${cluster}-${stage}-suze
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
llmSupport: ${LLM_SUPPORT:-false}

image:
  tag: "develop"

api:
  env:
    BACKEND_CORS_ORIGINS: https://portal.${domain}

appLifecycle:
  deployments:
    enabled: true
    cluster: ${cluster}
    repo: ${GENTIAN_DEPLOYMENTS_REPO:-https://github.com/gentian-org/gentian-deployments.git}
    gitCredentialsSecret: gentian-deployments-git-credentials
EOF
        info "Scaffolded ${kernel_dir}/values.yaml"
        generated=1
    fi

    # cluster-settings.env holds what KERNEL_DOMAIN and the stage cannot imply:
    # exposure model, tenancy, mail. The values written are the ones this run
    # resolved, so the file states the cluster's actual configuration rather
    # than a set of defaults the operator then has to guess at.
    if [[ ! -f "${kernel_dir}/cluster-settings.env" ]]; then
        cat > "${kernel_dir}/cluster-settings.env" <<EOF
# Runtime settings for cluster ${cluster} (stage ${stage}).
#
# KERNEL_DOMAIN is not here — it is authored in claims/cluster.yaml and nowhere
# else. Every option, including the ones left out below, is documented in
# gentian-os/cluster-settings.env.template.

# multi — one subdomain and Keycloak realm per tenant. single — one tenant
# occupies the whole cluster at KERNEL_DOMAIN.
TENANCY_MODE=${TENANCY_MODE:-multi}

# tunnel — reached through a reverse proxy or tunnel; NODE_IP is not needed.
# static-ip — DNS points straight at NODE_IP, which is then required.
NETWORK_MODE=${NETWORK_MODE:-tunnel}
$(if [[ -n "${NODE_IP:-}" ]]; then printf 'NODE_IP=%s\n' "${NODE_IP}"; else printf '# NODE_IP=203.0.113.10\n'; fi)
# gateway is the only supported value: Envoy Gateway plus the Gateway API.
ROUTING_MODE=${ROUTING_MODE:-gateway}

# external — relay through an external SMTP provider; needs EXTERNAL_SMTP_HOST
# here and SMTP_RELAY_USERNAME/PASSWORD in install.secrets.env. kernel — use
# in-cluster Postfix/Dovecot, which requires NETWORK_MODE=static-ip.
MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE:-external}
$(if [[ -n "${EXTERNAL_SMTP_HOST:-}" ]]; then printf 'EXTERNAL_SMTP_HOST=%s\n' "${EXTERNAL_SMTP_HOST}"; else printf '# EXTERNAL_SMTP_HOST=smtp.example.com\n'; fi)
# derived — every secret is HKDF-derived from MASTER_PASSWORD, so the whole
# cluster rotates from one value. random — each secret is independent.
SECRET_MODE=${SECRET_MODE:-derived}

# true when INFRA_CHART_REPO needs a login. false means the installer never asks
# for registry credentials, because a public chart URL has none to give.
INFRA_CHART_PRIVATE=${INFRA_CHART_PRIVATE:-false}

# Leave unset to use the cluster's default StorageClass.
$(if [[ -n "${STORAGE_CLASS:-}" ]]; then printf 'STORAGE_CLASS=%s\n' "${STORAGE_CLASS}"; else printf '# STORAGE_CLASS=nfs-csi\n'; fi)
EOF
        info "Scaffolded ${kernel_dir}/cluster-settings.env"
        generated=1
    fi

    if (( generated )); then
        echo ""
        success "Wrote clusters/${cluster}/kernel in ${GENTIAN_DEPLOYMENTS_PATH}."
        info "Nothing has been committed, pushed, or applied. Next:"
        info "  1. Read and edit clusters/${cluster}/kernel — the claims are what"
        info "     the cluster becomes, and cluster-settings.env carries the"
        info "     exposure and mail model."
        info "  2. Commit and push them to ${GENTIAN_DEPLOYMENTS_BRANCH:-main}."
        info "  3. Run ./install.sh"
    else
        info "clusters/${cluster}/kernel is already complete — nothing written."
    fi
}

# =============================================================================
# require_cluster_deployment — the forward pass's precondition.
#
# Installing reads this cluster's claims and values from gentian-deployments, so
# an absent file is not something to fill in silently: the generated default
# would decide the cluster's domain, tenancy and exposure model without anyone
# having read it. Name what is missing and stop.
# =============================================================================
require_cluster_deployment() {
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID}"
    local kernel_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel"
    local missing=() f

    # cluster-settings.env counts: it carries the exposure and mail model, which
    # nothing else can imply and which silently default to tunnel and external
    # when the file is absent.
    for f in claims/cluster.yaml claims/infra-data.yaml claims/suze.yaml \
             values.yaml cluster-settings.env; do
        [[ -f "${kernel_dir}/${f}" ]] || missing+=("${f}")
    done

    if (( ${#missing[@]} == 0 )); then
        return 0
    fi

    error "clusters/${cluster}/kernel is incomplete in gentian-deployments."
    error "  Missing: ${missing[*]}"
    error "  Path:    ${kernel_dir}"
    error ""
    error "  Generate them, review them, then install:"
    error "    ./install.sh --prepare-deployment"
    error "    (edit, commit and push clusters/${cluster}/kernel)"
    error "    ./install.sh"
    error ""
    error "  If this cluster's configuration lives elsewhere, check"
    error "  GENTIAN_DEPLOYMENTS_CLUSTER_ID and GENTIAN_DEPLOYMENTS_PATH."
    return 1
}
