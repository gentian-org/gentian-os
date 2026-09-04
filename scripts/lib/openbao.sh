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
    # The deployments token counts — it is prompted for on every run that does
    # not have it, so a fast path that ignores it skips the lookup that would
    # have prevented the prompt.
    MAIL_SERVICE_MODE="$(gentian_mail_service_mode)"
    # A role whose AUTH is "none" needs nothing here — the fast path must not
    # wait on a token that will never exist. _repo_auth_for is the same gate
    # _requirement_applies() uses, so this agrees with what
    # collect_bootstrap_credentials would actually prompt for.
    local _os_ready=1 _apps_ready=1 _ui_ready=1
    [[ "$(_repo_auth_for gentian-os-repository)" != "none" && -z "${GENTIAN_OS_GIT_TOKEN:-}" ]] && _os_ready=0
    [[ "$(_repo_auth_for gentian-apps-repository)" != "none" && -z "${GENTIAN_APPS_GIT_TOKEN:-}" ]] && _apps_ready=0
    [[ "$(_repo_auth_for gentian-ui-repository)" != "none" && -z "${GENTIAN_UI_GIT_TOKEN:-}" ]] && _ui_ready=0
    if [[ -n "${MASTER_PASSWORD:-}" && -n "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" \
        && "${_os_ready}" == "1" && "${_apps_ready}" == "1" && "${_ui_ready}" == "1" ]]; then
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
    #
    # Announced, because reaching it is not instant and everything up to here
    # was: gentian_service_addr probes the ClusterIP for up to 3s, and when the
    # host cannot route to the Service network it falls back to a port-forward
    # it polls for up to 30 iterations. On a cluster whose OpenBao is not up
    # that is a wordless minute directly after the config lines, which reads as
    # a hang rather than as work.
    info "Checking whether OpenBao already holds this cluster's credentials..."

    local bao_addr
    if ! bao_addr=$(gentian_service_addr openbao openbao 8200 https 2>/dev/null) \
        || [[ -z "${bao_addr}" ]]; then
        info "  OpenBao is not reachable yet — asking for the credentials instead."
        return 0
    fi
    export VAULT_SKIP_VERIFY=true

    # Don't bother if OpenBao is sealed/unreachable.
    if ! curl -k -sf --max-time 3 "${bao_addr}/v1/sys/health" >/dev/null 2>&1; then
        info "  OpenBao is not answering yet — asking for the credentials instead."
        return 0
    fi
    info "  OpenBao is reachable; reading what it already has."

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
    # The salt lives beside the password, and recovering one without the other
    # derives different credentials from the same input.
    if [[ -z "${DERIVATION_SALT:-}" ]]; then
        v=$(_bao_get "internal/master-password" '.data.data.salt')
        [[ -n "$v" ]] && { export DERIVATION_SALT="$v"; loaded=1; }
    fi
    # The rest of the bootstrap set. Without these, every run after B-10 removes
    # the local cache prompts again for credentials OpenBao already holds —
    # which is the cache's whole reason to exist, undone one step later.
    if [[ -z "${GENTIAN_DEPLOYMENTS_GIT_USERNAME:-}" ]]; then
        v=$(_bao_get "repositories/deployments" '.data.data.username')
        [[ -n "$v" ]] && { export GENTIAN_DEPLOYMENTS_GIT_USERNAME="$v"; loaded=1; }
    fi
    if [[ -z "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" ]]; then
        v=$(_bao_get "repositories/deployments" '.data.data.password')
        [[ -n "$v" ]] && { export GENTIAN_DEPLOYMENTS_GIT_TOKEN="$v"; loaded=1; }
    fi
    # os/apps/ui mirror the deployments pair above, but only when their AUTH
    # gates them in — reading a path nothing ever wrote is a guaranteed 404,
    # every run, for the common unmirrored install.
    if [[ "$(_repo_auth_for gentian-os-repository)" != "none" ]]; then
        if [[ -z "${GENTIAN_OS_GIT_USERNAME:-}" ]]; then
            v=$(_bao_get "repositories/os" '.data.data.username')
            [[ -n "$v" ]] && { export GENTIAN_OS_GIT_USERNAME="$v"; loaded=1; }
        fi
        if [[ -z "${GENTIAN_OS_GIT_TOKEN:-}" ]]; then
            v=$(_bao_get "repositories/os" '.data.data.password')
            [[ -n "$v" ]] && { export GENTIAN_OS_GIT_TOKEN="$v"; loaded=1; }
        fi
    fi
    if [[ "$(_repo_auth_for gentian-apps-repository)" != "none" ]]; then
        if [[ -z "${GENTIAN_APPS_GIT_USERNAME:-}" ]]; then
            v=$(_bao_get "repositories/apps" '.data.data.username')
            [[ -n "$v" ]] && { export GENTIAN_APPS_GIT_USERNAME="$v"; loaded=1; }
        fi
        if [[ -z "${GENTIAN_APPS_GIT_TOKEN:-}" ]]; then
            v=$(_bao_get "repositories/apps" '.data.data.password')
            [[ -n "$v" ]] && { export GENTIAN_APPS_GIT_TOKEN="$v"; loaded=1; }
        fi
    fi
    if [[ "$(_repo_auth_for gentian-ui-repository)" != "none" ]]; then
        if [[ -z "${GENTIAN_UI_GIT_USERNAME:-}" ]]; then
            v=$(_bao_get "repositories/ui" '.data.data.username')
            [[ -n "$v" ]] && { export GENTIAN_UI_GIT_USERNAME="$v"; loaded=1; }
        fi
        if [[ -z "${GENTIAN_UI_GIT_TOKEN:-}" ]]; then
            v=$(_bao_get "repositories/ui" '.data.data.password')
            [[ -n "$v" ]] && { export GENTIAN_UI_GIT_TOKEN="$v"; loaded=1; }
        fi
    fi
    if [[ -z "${REGISTRY_USER:-}" ]]; then
        v=$(_bao_get "storage/registry" '.data.data.username')
        [[ -n "$v" ]] && { export REGISTRY_USER="$v"; loaded=1; }
    fi
    if [[ -z "${REGISTRY_PASSWORD:-}" ]]; then
        v=$(_bao_get "storage/registry" '.data.data.password')
        [[ -n "$v" ]] && { export REGISTRY_PASSWORD="$v"; loaded=1; }
    fi
    if [[ -z "${CF_API_TOKEN:-}" ]]; then
        # Bracket notation: jq reads a hyphen in a bare path as subtraction.
        v=$(_bao_get "dns/cloudflare" '.data.data["api-token"]')
        [[ -n "$v" ]] && { export CF_API_TOKEN="$v"; loaded=1; }
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
    banner "Installing External Secrets Operator"

    if helm status external-secrets -n external-secrets &>/dev/null; then
        success "ESO already installed. Skipping."
        return
    fi

    helm repo add external-secrets "$(gentian_pin external-secrets repo)" --force-update
    helm repo update external-secrets
    _helm_retry install external-secrets external-secrets/external-secrets \
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
    banner "OpenBao transit seal instance"

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
        error "Argo CD did not deploy openbao-transit StatefulSet."
        exit 1
    }

    if ! wait_for_running_pod openbao "app.kubernetes.io/instance=openbao-transit" "openbao-transit" 480; then
        error "openbao-transit pod never became Ready. Aborting install."
        exit 1
    fi
}

# =============================================================================
# 5b. Init the transit instance
# =============================================================================
init_openbao_transit() {
    banner "Transit instance init + autounseal Secret"
    if ! bash "${SCRIPT_DIR}/scripts/bootstrap/init-openbao-transit.sh"; then
        error "init-openbao-transit.sh exited non-zero."
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
        error "Transit init reported success but required Secrets are missing: ${missing[*]}"
        error "Re-run init-openbao-transit.sh manually and re-run install.sh."
        exit 1
    fi
}
# =============================================================================
# 7. Initialize primary OpenBao (transit auto-unseal)
# =============================================================================
init_openbao() {
    banner "OpenBao init"

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
        # Report the STATE of the stored credentials on re-runs — never their
        # values. This block used to re-print both on every single install.sh
        # invocation until E-04 revoked the token, which made "how many times
        # has this value been in a terminal or a CI log" grow with every
        # re-run rather than stay at one. Nothing here needs the literal text:
        # BAO_TOKEN is exported for this shell's own later steps to use, which
        # needs the value in the environment, not on the screen.
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            local stored_token
            stored_token=$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null)
            if [[ -n "$stored_token" ]]; then
                # E-04 revokes this token at handover, but this file outlives
                # that. Exporting it unasked made every later OpenBao write die
                # on a bare 403 — so ask OpenBao first, and when it is dead say
                # which kind of dead: revoked on purpose, or orphaned by a
                # re-initialisation.
                if curl -k -sf -H "X-Vault-Token: ${stored_token}" \
                        "${BAO_HTTP}/v1/auth/token/lookup-self" >/dev/null; then
                    info "Bootstrap token: live, in ${OPENBAO_INIT_FILE} (mode 600)."
                    if [[ "$(kubectl get configmap gentian-handover \
                            -n "${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}" \
                            -o jsonpath='{.data.recoveryKitExported}' 2>/dev/null)" != "true" ]]; then
                        warn "  No recovery kit is on record yet. Run:"
                        warn "    ./install.sh --export-recovery-kit"
                    fi
                    export BAO_TOKEN="$stored_token"
                elif [[ "$(kubectl get configmap gentian-handover \
                        -n "${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}" \
                        -o jsonpath='{.data.bootstrapCredentialRevoked}' 2>/dev/null)" == "true" ]]; then
                    info "Bootstrap token: revoked at handover (E-04)."
                    info "  Day-2 writes go through OIDC; steps that need an OpenBao"
                    info "  token will report undefined and skip."
                else
                    warn "Bootstrap token: no longer authenticates, and no revocation"
                    warn "  is recorded — OpenBao was likely re-initialised since this"
                    warn "  file was written. If you still hold the recovery key from"
                    warn "  when this cluster was first initialised, mint a new root"
                    warn "  token with 'bao operator generate-root', export it as"
                    warn "  BAO_TOKEN, and re-run. Otherwise recovery is from a kit."
                fi
            fi
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

        # The directory, before the only copy of the recovery key is written
        # into it. Everything else that writes here runs earlier in a normal
        # install, so this is belt and braces — but the one write that must
        # never fail for want of a directory is this one.
        mkdir -p "$(dirname "${OPENBAO_INIT_FILE}")"
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

        # Neither value is printed. Both are already in ${OPENBAO_INIT_FILE},
        # mode 600, which is where they have always actually lived — the
        # banner this replaced was a second, unprotected copy of exactly the
        # same values, in the one place (a terminal, a CI log) they should
        # never sit in the clear. The durable copy is a recovery kit, and nothing
        # downstream of here needs the raw text: E-04 later refuses to revoke
        # this token until `./install.sh --export-recovery-kit` has run.
        echo ""
        info "OpenBao initialised. The recovery key and root token are in"
        info "  ${OPENBAO_INIT_FILE} (mode 600) — nowhere else."
        warn "Before this cluster can finish handover, run:"
        warn "    ./install.sh --export-recovery-kit"
        echo ""

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

        # The directory, before the only copy of the recovery key is written
        # into it. Everything else that writes here runs earlier in a normal
        # install, so this is belt and braces — but the one write that must
        # never fail for want of a directory is this one.
        mkdir -p "$(dirname "${OPENBAO_INIT_FILE}")"
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

        # Same reasoning as the transit-seal branch above: not printed, both
        # already in the mode-600 init file, and a recovery kit — not this
        # terminal — is where a durable copy belongs.
        echo ""
        info "OpenBao initialised. The unseal key and root token are in"
        info "  ${OPENBAO_INIT_FILE} (mode 600) — nowhere else."
        warn "Before this cluster can finish handover, run:"
        warn "    ./install.sh --export-recovery-kit"
        echo ""

        curl -k -sf -X PUT "${BAO_HTTP}/v1/sys/unseal" \
            -H "Content-Type: application/json" \
            -d "{\"key\": \"${unseal_key}\"}" | jq .
        export BAO_TOKEN="$root_token"
        success "OpenBao initialized and unsealed (Shamir)."
    fi
}

# =============================================================================
# 10b. Seed kernel secrets
# _dns_credential_fields_json — the active DNS provider's fields as one object.
#
# Field names come from the catalogue, which reads them from
# kernel/platforms.yaml, and values from the environment variables the prompt
# loop wrote. Empty when the provider is Cloudflare (which has its own
# variables, kept for compatibility), "none", or when nothing was collected.
_dns_credential_fields_json() {
    local provider="${DNS_PROVIDER:-cloudflare}" key var value args=()
    [[ "${provider}" == "cloudflare" || "${provider}" == "none" ]] && return 0
    while IFS= read -r key; do
        [[ -n "${key}" ]] || continue
        var="$(_env_var_for "acme-dns-${provider}" "${key}")"
        [[ -n "${var}" ]] || continue
        value="${!var:-}"
        [[ -n "${value}" ]] || continue
        args+=(--arg "${key}" "${value}")
    done < <(catalogue_field_keys "acme-dns-${provider}")
    [[ ${#args[@]} -gt 0 ]] || return 0
    jq -n "${args[@]}" '$ARGS.named'
}

# =============================================================================
# _resolve_bao_token — get a working OpenBao token for a write, preferring the
# least powerful option that will do it. Assumes BAO_ADDR is already exported
# (every caller resolves it immediately before reaching here).
#
# Order: already exported, the bootstrap-only init file, an interactive OIDC
# sign-in as cluster-admin, and only then the raw root-token prompt.
#
# The OIDC tier exists for exactly one situation: a cluster that has been
# handed over. E-04-revoke-bootstrap-token deliberately revokes the root
# token and strips it from both the openbao-init Secret and this file, so the
# first two tiers come up empty on any post-handover re-run — asking for the
# root token at that point is asking for something that cannot exist. It sits
# ahead of the prompt, not in place of it: a fresh cluster before handover, or
# one whose OIDC is itself broken, still has the root token as a working
# fallback, so this tries the better option first and only asks for the worse
# one if it fails.
#
# The role and its policy already exist for this: cluster-default.yaml binds
# OpenBao's cluster-admin OIDC role to localhost:8250 (the CLI's own callback
# port) with tokenPolicies: [cluster-admin], and that policy already grants
# create/update on secret/data/gentian-os/kernel/* — the same tree every
# caller of this function writes to. Nothing new to configure; `bao login` is
# a stock OpenBao feature this role was already shaped to support.
_resolve_bao_token() {
    [[ -n "${BAO_TOKEN:-}" ]] && return 0

    if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
        BAO_TOKEN="$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null)"
        if [[ -n "${BAO_TOKEN}" ]]; then
            export BAO_TOKEN
            return 0
        fi
    fi

    if command -v bao >/dev/null 2>&1 && [[ -n "${BAO_ADDR:-}" ]]; then
        info "No OpenBao token available; trying an OIDC sign-in as cluster-admin..."
        info "  A browser should open. Sign in as the cluster administrator."
        # -token-only: the token and nothing else on stdout (no verification
        # banner, no wrapping details), and it is not written to the local
        # token helper file — this shell carries it as BAO_TOKEN like every
        # other source here, not as a second credential left on disk.
        # stderr is left unredirected so bao's own "opening your browser…"/URL
        # output still reaches the terminal; only stdout captures the token.
        #
        # Bounded at 2 minutes without the external timeout(1) — not portable,
        # and this installer runs on stock macOS too (lint-portability). A
        # background job, polled and killed on its own PID, does the same job
        # with nothing beyond bash builtins: unattended (no browser, nobody
        # watching) this would otherwise hang until OpenBao's own login
        # timeout, which is longer than the prompt it exists to avoid.
        local oidc_token="" oidc_out oidc_pid oidc_waited=0
        oidc_out="$(mktemp)"
        bao login -method=oidc -path=oidc -token-only role=cluster-admin >"${oidc_out}" &
        oidc_pid=$!
        while kill -0 "${oidc_pid}" 2>/dev/null && [[ ${oidc_waited} -lt 120 ]]; do
            sleep 1
            oidc_waited=$((oidc_waited + 1))
        done
        if kill -0 "${oidc_pid}" 2>/dev/null; then
            kill "${oidc_pid}" 2>/dev/null
            wait "${oidc_pid}" 2>/dev/null
            warn "OIDC sign-in timed out after 120s."
        elif wait "${oidc_pid}"; then
            oidc_token="$(cat "${oidc_out}")"
        fi
        rm -f "${oidc_out}"

        if [[ -n "${oidc_token}" ]]; then
            BAO_TOKEN="${oidc_token}"
            export BAO_TOKEN
            success "Signed in via OIDC."
            return 0
        fi
        warn "OIDC sign-in did not complete; falling back to the root token."
    fi

    read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
    export BAO_TOKEN
}

# =============================================================================
# resolve_openbao_access — point at OpenBao and get a token, for a read-only
# command that is not part of an install.
#
# seed_secrets does the same three lines inline, but it exits on failure because
# an install that cannot seed is over. A command that only reads must not: the
# caller reports what it could not gather, which is a better message than an
# address lookup's.
#
# Returns non-zero when OpenBao cannot be reached, leaving BAO_TOKEN unset.
resolve_openbao_access() {
    if ! BAO_ADDR=$(gentian_service_addr openbao openbao 8200 https); then
        warn "Could not reach the openbao Service on :8200."
        warn "  Neither the ClusterIP nor a kubectl port-forward responded."
        return 1
    fi
    export BAO_ADDR
    export VAULT_SKIP_VERIFY=true
    _resolve_bao_token
}

# =============================================================================
seed_secrets() {
    banner "Seeding kernel secrets"

    if ! BAO_ADDR=$(gentian_service_addr openbao openbao 8200 https); then
        error "Could not reach the openbao Service on :8200."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        exit 1
    fi
    export BAO_ADDR
    export VAULT_SKIP_VERIFY=true

    _resolve_bao_token

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

        # A Cloudflare Tunnel only exists in NETWORK_MODE=tunnel. In static-ip
        # mode DNS points straight at NODE_IP, so there is no tunnel to resolve
        # and looking for one just produces a spurious warning.
        if [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]]; then
            info "NETWORK_MODE=static-ip: no Cloudflare Tunnel to resolve (DNS points at NODE_IP)."
        else
            info "Resolving in-cluster Cloudflare Tunnel ID..."
            local tunnel_id=""
            local token_val
            token_val=$(kubectl get secret cf-tunnel -n default -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null | base64 -d 2>/dev/null || true)
            if [[ -n "${token_val}" ]]; then
                tunnel_id=$(echo "${token_val}" | jq -r '.t // empty' || true)
            fi
            if [[ -z "${tunnel_id}" ]]; then
                # `|| true` is load-bearing: kubectl exits non-zero when the
                # Secret is absent, and 2>/dev/null hides the message but not the
                # status. Under `set -o pipefail` that aborted the whole install
                # with no output at all.
                tunnel_id=$(kubectl get secret tunnel-credentials -n default -o jsonpath='{.data}' 2>/dev/null \
                    | jq -r 'keys[0] // empty' 2>/dev/null \
                    | sed 's/\.json$//' || true)
            fi
            if [[ -n "${tunnel_id}" ]]; then
                tunnel_cname="${tunnel_id}.cfargotunnel.com"
                info "Resolved Cloudflare Tunnel CNAME: ${tunnel_cname}"
            else
                warn "Could not resolve Cloudflare Tunnel ID from tunnel-credentials secret"
            fi
        fi
    fi

    # A provider other than Cloudflare hands its fields over as one JSON object,
    # built from the catalogue rather than from a variable per provider — the
    # installer already knows which fields the provider declares, and a second
    # list here would be the place they stop matching.
    local dns_fields_json=""
    dns_fields_json="$(_dns_credential_fields_json)"

    # CF_API_TOKEN is forwarded via env var (not positional) so the
    # seed-openbao.sh contract stays backward-compatible. Seed-openbao
    # writes it to secret/gentian-os/kernel/dns/<provider> when present.
    DNS_PROVIDER="${DNS_PROVIDER:-cloudflare}" \
    GENTIAN_DNS_FIELDS_JSON="${dns_fields_json}" \
    CF_API_TOKEN="${CF_API_TOKEN:-}" \
    CF_ZONE_ID="${zone_id}" \
    CF_TUNNEL_CNAME="${tunnel_cname}" \
    MAIL_SERVICE_MODE="$(gentian_mail_service_mode)" \
    EXTERNAL_SMTP_HOST="${EXTERNAL_SMTP_HOST:-}" \
    EXTERNAL_SMTP_PORT="${EXTERNAL_SMTP_PORT:-587}" \
    EXTERNAL_SMTP_SSL="${EXTERNAL_SMTP_SSL:-false}" \
    EXTERNAL_SMTP_STARTTLS="${EXTERNAL_SMTP_STARTTLS:-true}" \
    bash "${SCRIPT_DIR}/scripts/bootstrap/seed-openbao.sh" \
        "$MASTER_PASSWORD" \
        "${SMTP_RELAY_USERNAME:-}" \
        "${SMTP_RELAY_PASSWORD:-}"
    success "All kernel secrets seeded."
}
