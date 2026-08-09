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

    # Need a reachable OpenBao service. Skip silently if not yet deployed, or if
    # neither the ClusterIP nor a port-forward answers — this is a best-effort
    # convenience path, so it must never abort the install.
    local bao_addr
    bao_addr=$(gentian_service_addr openbao openbao 8200 https 2>/dev/null) || return 0
    [[ -n "${bao_addr}" ]] || return 0
    export VAULT_SKIP_VERIFY=true

    # Don't bother if OpenBao is sealed/unreachable.
    curl -k -sf --max-time 3 "${bao_addr}/v1/sys/health" >/dev/null 2>&1 || return 0

    _bao_get() {
        # $1 = relative path under secret/data/gentian-os/kernel/
        # $2 = jq filter to extract the field, e.g. '.data.data.value'
        # Missing paths (404) are normal before OpenBao is bootstrapped — must not abort install.
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

    apply_bootstrap_application openbao-transit
    success "Applied openbao-transit-application.yaml (storageClass=${STORAGE_CLASS})"

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

    local BAO_HTTP
    if ! BAO_HTTP=$(gentian_service_addr openbao openbao 8200 https); then
        error "Could not reach the openbao Service on :8200."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        exit 1
    fi
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
# 10b. Seed kernel secrets
# =============================================================================
seed_secrets() {
    banner "Step 10b — Seeding kernel secrets"

    if ! BAO_ADDR=$(gentian_service_addr openbao openbao 8200 https); then
        error "Could not reach the openbao Service on :8200."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        exit 1
    fi
    export BAO_ADDR
    export VAULT_SKIP_VERIFY=true

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "${OPENBAO_INIT_FILE}")
        else
            read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export BAO_TOKEN

    # Automatically query Cloudflare zone ID and tunnel CNAME to seed into OpenBao
    local zone_id=""
    local tunnel_cname=""
    if [[ -n "${CF_API_TOKEN:-}" && -n "${KERNEL_DOMAIN:-}" ]]; then
        info "Resolving Cloudflare Zone ID for domain ${KERNEL_DOMAIN}..."
        zone_id=$(curl -s -X GET "https://api.cloudflare.com/client/v4/zones?name=${KERNEL_DOMAIN}" \
            -H "Authorization: Bearer ${CF_API_TOKEN}" | jq -r '.result[0].id // empty')
        if [[ -z "${zone_id}" && "${KERNEL_DOMAIN}" == *.*.* ]]; then
            local apex_domain
            apex_domain=$(echo "${KERNEL_DOMAIN}" | awk -F. '{print $(NF-1)"."$NF}')
            zone_id=$(curl -s -X GET "https://api.cloudflare.com/client/v4/zones?name=${apex_domain}" \
                -H "Authorization: Bearer ${CF_API_TOKEN}" | jq -r '.result[0].id // empty')
        fi
        if [[ -n "${zone_id}" ]]; then
            info "Resolved Cloudflare Zone ID: ${zone_id}"
        else
            warn "Could not resolve Cloudflare Zone ID for ${KERNEL_DOMAIN}"
        fi

        info "Resolving in-cluster Cloudflare Tunnel ID..."
        local tunnel_id=""
        local token_val
        token_val=$(kubectl get secret cf-tunnel -n default -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null | base64 -d 2>/dev/null || true)
        if [[ -n "${token_val}" ]]; then
            tunnel_id=$(echo "${token_val}" | jq -r '.t // empty')
        fi
        if [[ -z "${tunnel_id}" ]]; then
            tunnel_id=$(kubectl get secret tunnel-credentials -n default -o jsonpath='{.data}' 2>/dev/null | jq -r 'keys[0] // empty' | sed 's/\.json$//')
        fi
        if [[ -n "${tunnel_id}" ]]; then
            tunnel_cname="${tunnel_id}.cfargotunnel.com"
            info "Resolved Cloudflare Tunnel CNAME: ${tunnel_cname}"
        else
            warn "Could not resolve Cloudflare Tunnel ID from tunnel-credentials secret"
        fi
    fi

    # CF_API_TOKEN is forwarded via env var (not positional) so the
    # seed-openbao.sh contract stays backward-compatible. Seed-openbao
    # writes it to secret/gentian-os/kernel/dns/cloudflare when present.
    CF_API_TOKEN="${CF_API_TOKEN:-}" \
    CF_ZONE_ID="${zone_id}" \
    CF_TUNNEL_CNAME="${tunnel_cname}" \
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
