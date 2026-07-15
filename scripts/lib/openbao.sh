#!/usr/bin/env bash
# =============================================================================
# scripts/lib/openbao.sh — OpenBao transit seal, init, bootstrap, ESO, and secret seeding.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# Try to load any missing credentials from a previously seeded OpenBao before
# prompting the operator. Useful on re-runs: the operator only has to provide
# secrets the first time. Silently skipped if OpenBao is not yet reachable
# (e.g. on the very first run).
# =============================================================================
try_load_creds_from_openbao() {
    # Fast path: if everything required for this mail mode is exported, skip.
    MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
    if [[ -n "${MASTER_PASSWORD:-}" ]]; then
        if [[ "${MAIL_SERVICE_MODE}" == "external" \
            && -n "${SMTP_RELAY_USERNAME:-}" \
            && -n "${SMTP_RELAY_PASSWORD:-}" ]]; then
            return
        fi
        if [[ "${MAIL_SERVICE_MODE}" == "kernel" ]]; then
            return
        fi
    fi

    # Need a root token to read secrets. Prefer env, fall back to init file.
    local token=""
    if [[ -n "${BAO_TOKEN:-}" ]]; then
        token="$BAO_TOKEN"
    elif [[ -f "${OPENBAO_INIT_FILE}" ]]; then
        token=$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null || true)
    fi
    [[ -n "$token" ]] || return 0

    # Need a reachable OpenBao service. Skip silently if not yet deployed.
    local bao_ip
    bao_ip=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    [[ -n "$bao_ip" ]] || return 0
    local bao_addr="https://${bao_ip}:8200"
    export VAULT_SKIP_VERIFY=true

    # Don't bother if OpenBao is sealed/unreachable.
    curl -k -sf --max-time 3 "${bao_addr}/v1/sys/health" >/dev/null 2>&1 || return 0

    _bao_get() {
        # $1 = relative path under secret/data/gentian-os/kernel/
        # $2 = jq filter to extract the field, e.g. '.data.data.value'
        # Missing paths (404) are normal before bao_bootstrap — must not abort install.
        curl -k -sf --max-time 5 \
            -H "X-Vault-Token: ${token}" \
            "${bao_addr}/v1/secret/data/gentian-os/kernel/$1" 2>/dev/null \
            | jq -r "$2 // empty" 2>/dev/null || true
    }

    local loaded=0 v
    if [[ -z "${MASTER_PASSWORD:-}" ]]; then
        v=$(_bao_get "internal/master-password" '.data.data.value')
        [[ -n "$v" ]] && { export MASTER_PASSWORD="$v"; loaded=1; }
    fi
    if [[ -z "${SMTP_RELAY_USERNAME:-}" ]]; then
        v=$(_bao_get "mail/postfix" '.data.data.relay_username')
        [[ -n "$v" ]] && { export SMTP_RELAY_USERNAME="$v"; loaded=1; }
    fi
    if [[ -z "${SMTP_RELAY_PASSWORD:-}" ]]; then
        v=$(_bao_get "mail/postfix" '.data.data.relay_password')
        [[ -n "$v" ]] && { export SMTP_RELAY_PASSWORD="$v"; loaded=1; }
    fi

    if [[ "$loaded" -eq 1 ]]; then
        info "Loaded missing credentials from OpenBao."
    fi
}
install_eso() {
    banner "Step 3 — Installing External Secrets Operator"

    if helm status external-secrets -n external-secrets &>/dev/null; then
        success "ESO already installed. Skipping."
        return
    fi

    helm repo add external-secrets https://charts.external-secrets.io --force-update
    helm repo update
    helm install external-secrets external-secrets/external-secrets \
        -n external-secrets \
        --version "${ESO_CHART_VERSION}" \
        -f "${SCRIPT_DIR}/kernel/eso/values.yaml" \
        --wait --timeout 5m
    success "ESO installed."
}
# =============================================================================
# 5. Deploy OpenBao transit seal instance
# =============================================================================
bootstrap_transit_app() {
    banner "Step 5 — OpenBao transit seal instance"

    # Note: CRI cleanup is intentionally NOT run here pre-flight. It is
    # invoked reactively by wait_for_running_pod's 2nd-tier escalation
    # only if the transit pod is demonstrably wedged (stuck 120s+ in
    # ContainerCreating with no IP), so a fresh / healthy cluster never
    # pays the sudo-prompt + sweep cost.

    if ! kubectl get secret openbao-transit-unseal -n openbao &>/dev/null; then
        kubectl create secret generic openbao-transit-unseal \
            -n openbao --from-literal=unseal-key=placeholder
        success "Placeholder openbao-transit-unseal secret created."
    fi

    kubectl apply -f "${SCRIPT_DIR}/kernel/bootstrap/openbao-transit-application.yaml"
    success "Applied openbao-transit-application.yaml"

    _wait_for_argocd_application_workload \
        openbao-transit openbao statefulset \
        "app.kubernetes.io/instance=openbao-transit" 300 \
    || {
        error "Step 5 failed: Argo CD did not deploy openbao-transit StatefulSet."
        exit 1
    }

    if ! wait_for_running_pod openbao "app.kubernetes.io/instance=openbao-transit" "openbao-transit" 480; then
        error "Step 5 failed: openbao-transit pod never became Ready. Aborting install."
        exit 1
    fi
}

# =============================================================================
# 5b. Init the transit instance
# =============================================================================
init_openbao_transit() {
    banner "Step 5b — Transit instance init + autounseal Secret"
    if ! bash "${SCRIPT_DIR}/scripts/init-openbao-transit.sh"; then
        error "Step 5b failed: init-openbao-transit.sh exited non-zero."
        error "Without the openbao-transit-token Secret, the primary OpenBao"
        error "will be stuck in CreateContainerConfigError. Aborting install."
        exit 1
    fi
    # Sanity-check the side effects the script is supposed to produce. If
    # the script exited 0 but didn't actually create both Secrets (e.g. it
    # silently took an early-return path on a stale state), fail fast here
    # so subsequent steps don't proceed against a half-initialised transit.
    local missing=()
    kubectl get secret -n openbao openbao-transit-token  >/dev/null 2>&1 || missing+=(openbao-transit-token)
    kubectl get secret -n openbao openbao-transit-unseal >/dev/null 2>&1 || missing+=(openbao-transit-unseal)
    if (( ${#missing[@]} > 0 )); then
        error "Step 5 reported success but required Secrets are missing: ${missing[*]}"
        error "Re-run init-openbao-transit.sh manually and re-run install.sh."
        exit 1
    fi
}
# =============================================================================
# 7. Initialize primary OpenBao (transit auto-unseal)
# =============================================================================
init_openbao() {
    banner "Step 7 — OpenBao init"

    info "Waiting for openbao service (up to 2 min)..."
    local i=0
    until kubectl get svc openbao -n openbao &>/dev/null; do
        echo -n "."; sleep 5; i=$((i + 5))
        [[ $i -lt 120 ]] || { error "Timed out."; exit 1; }
    done
    echo ""

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    local BAO_HTTP="https://${BAO_SVC_IP}:8200"
    export VAULT_SKIP_VERIFY=true

    local init_status
    init_status=$(curl -k -sf "${BAO_HTTP}/v1/sys/init" | jq -r '.initialized')

    if [[ "$init_status" == "true" ]]; then
        success "OpenBao already initialized."
        local sealed seal_type
        sealed=$(curl -k -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.sealed')
        seal_type=$(curl -k -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.type')
        if [[ "$sealed" == "true" && "$seal_type" == "transit" ]]; then
            warn "OpenBao sealed — waiting for transit auto-unseal..."
            sleep 15
            sealed=$(curl -k -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.sealed')
            [[ "$sealed" == "true" ]] && { error "Auto-unseal failed."; exit 1; }
            success "Transit auto-unseal completed."
        fi
        # Re-display stored credentials so the operator can verify them on re-runs.
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            local stored_key stored_token
            stored_key=$(jq -r '(.recovery_keys_base64 // .recovery_keys_b64 // .keys_base64 // [])[0] // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null)
            stored_token=$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null)
            info "Stored init credentials (${OPENBAO_INIT_FILE}):"
            [[ -n "$stored_key"   ]] && info "  Recovery/Unseal Key : ${stored_key}"
            [[ -n "$stored_token" ]] && info "  Root Token          : ${stored_token}"
            [[ -n "$stored_token" ]] && export BAO_TOKEN="$stored_token"
        fi
        return
    fi

    local seal_type_before
    seal_type_before=$(curl -k -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.type')

    if [[ "$seal_type_before" == "transit" ]]; then
        info "Initializing OpenBao with transit seal (recovery_shares=1)..."
        local init_resp
        init_resp=$(curl -k -sf -X PUT "${BAO_HTTP}/v1/sys/init" \
            -H "Content-Type: application/json" \
            -d '{"recovery_shares": 1, "recovery_threshold": 1}')

        echo "$init_resp" | jq '.' > "${OPENBAO_INIT_FILE}"
        chmod 600 "${OPENBAO_INIT_FILE}"

        local recovery_key root_token
        recovery_key=$(echo "$init_resp" | jq -r '(.recovery_keys_base64 // .recovery_keys_b64 // .recovery_keys // [])[0] // empty')
        root_token=$(echo "$init_resp"   | jq -r '.root_token // empty')

        if [[ -z "$recovery_key" || -z "$root_token" ]]; then
            error "Failed to parse OpenBao init response. Full payload saved at ${OPENBAO_INIT_FILE}."
            echo "$init_resp" | jq . >&2 || echo "$init_resp" >&2
            exit 1
        fi

        echo ""
        echo -e "${RED}╔═══════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${RED}║  ⚠  SAVE THESE VALUES (password manager)                     ║${NC}"
        echo -e "${RED}╠═══════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${RED}║  Recovery Key (= unseal key) : ${recovery_key}${NC}"
        echo -e "${RED}║  Root Token                  : ${root_token}${NC}"
        echo -e "${RED}╚═══════════════════════════════════════════════════════════════╝${NC}"
        echo ""
        read -rp "  Saved both values? [yes/no]: " confirmed
        [[ "$confirmed" == "yes" ]] || { error "Aborted."; exit 1; }

        export BAO_TOKEN="$root_token"

        info "Waiting for transit auto-unseal (up to 30 s)..."
        i=0
        until curl -k -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -e '.sealed == false' >/dev/null 2>&1; do
            sleep 3; i=$((i + 3))
            [[ $i -lt 30 ]] || { error "Auto-unseal timed out."; exit 1; }
        done
        success "OpenBao initialized and auto-unsealed via transit."
    else
        info "Initializing OpenBao (1-of-1 Shamir — transit unavailable)..."
        local init_resp
        init_resp=$(curl -k -sf -X PUT "${BAO_HTTP}/v1/sys/init" \
            -H "Content-Type: application/json" \
            -d '{"secret_shares": 1, "secret_threshold": 1}') || {
            error "OpenBao init request failed against ${BAO_HTTP}."
            error "The openbao-0 pod likely has no Ready endpoints (check 'kubectl get pod -n openbao')."
            error "Common cause: the openbao-transit-token Secret is missing, leaving openbao-0 in CreateContainerConfigError."
            exit 1
        }

        echo "$init_resp" | jq '.' > "${OPENBAO_INIT_FILE}"
        chmod 600 "${OPENBAO_INIT_FILE}"

        local unseal_key root_token
        unseal_key=$(echo "$init_resp" | jq -r '.keys_base64[0] // empty')
        root_token=$(echo "$init_resp"  | jq -r '.root_token // empty')

        if [[ -z "$unseal_key" || -z "$root_token" ]]; then
            error "OpenBao init response missing keys_base64[0] or root_token."
            error "Raw response saved at ${OPENBAO_INIT_FILE}."
            echo "$init_resp" | jq . >&2 || echo "$init_resp" >&2
            exit 1
        fi

        echo ""
        echo -e "${RED}║  Unseal Key : ${unseal_key}${NC}"
        echo -e "${RED}║  Root Token : ${root_token}${NC}"
        read -rp "  Saved both values? [yes/no]: " confirmed
        [[ "$confirmed" == "yes" ]] || { error "Aborted."; exit 1; }

        curl -k -sf -X PUT "${BAO_HTTP}/v1/sys/unseal" \
            -H "Content-Type: application/json" \
            -d "{\"key\": \"${unseal_key}\"}" | jq .
        export BAO_TOKEN="$root_token"
        success "OpenBao initialized and unsealed (Shamir)."
    fi
}

# =============================================================================
# 11. Bootstrap OpenBao via bao CLI (KV engine, K8s auth, policies, roles)
#
# Creates the minimal permanent resources that the rest of the install needs:
#   • KV v2 mount at secret/
#   • Kubernetes auth backend + config
#   • eso-read policy + eso role  (ESO ClusterSecretStore authentication)
#
# The operator-write policy and gentian-os-operator role are NOT created here;
# they are managed as provider-vault Crossplane MRs in
# kernel/services/openbao-config/manifests/{env}/ (wave 15).
# =============================================================================
bao_bootstrap() {
    banner "Step 11 — OpenBao bootstrap (bao CLI)"

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
    export VAULT_TOKEN="$BAO_TOKEN"

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

    # ── 3. eso-read policy ────────────────────────────────────────────────────
    bao policy write eso-read - <<'POLICY'
path "secret/data/gentian-os/kernel/*"          { capabilities = ["read"] }
path "secret/metadata/gentian-os/kernel/*"      { capabilities = ["list"] }
path "secret/data/gentian-os/tenants/+/apps/*" { capabilities = ["read"] }
path "secret/metadata/gentian-os/tenants/*"     { capabilities = ["list"] }
POLICY
    success "eso-read policy written."

    # ── 4. eso Kubernetes auth role ───────────────────────────────────────────
    bao write auth/kubernetes/role/eso \
        bound_service_account_names=external-secrets \
        bound_service_account_namespaces=external-secrets \
        token_policies=eso-read \
        token_ttl=3600
    success "eso K8s auth role created."

    success "OpenBao bootstrap complete."
}

# =============================================================================
# 10b. Seed kernel secrets
# =============================================================================
seed_secrets() {
    banner "Step 10b — Seeding kernel secrets"

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    export BAO_ADDR="https://${BAO_SVC_IP}:8200"
    export VAULT_SKIP_VERIFY=true

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "${OPENBAO_INIT_FILE}")
        else
            read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export BAO_TOKEN

    # CF_API_TOKEN is forwarded via env var (not positional) so the
    # seed-openbao.sh contract stays backward-compatible. Seed-openbao
    # writes it to secret/gentian-os/kernel/dns/cloudflare when present.
    CF_API_TOKEN="${CF_API_TOKEN:-}" \
    MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}" \
    EXTERNAL_SMTP_HOST="${EXTERNAL_SMTP_HOST:-}" \
    EXTERNAL_SMTP_PORT="${EXTERNAL_SMTP_PORT:-587}" \
    EXTERNAL_SMTP_SSL="${EXTERNAL_SMTP_SSL:-false}" \
    EXTERNAL_SMTP_STARTTLS="${EXTERNAL_SMTP_STARTTLS:-true}" \
    bash "${SCRIPT_DIR}/scripts/seed-openbao.sh" \
        "$MASTER_PASSWORD" \
        "${SMTP_RELAY_USERNAME:-}" \
        "${SMTP_RELAY_PASSWORD:-}"
    success "All kernel secrets seeded."
}
