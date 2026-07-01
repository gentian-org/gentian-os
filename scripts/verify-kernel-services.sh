#!/usr/bin/env bash
# =============================================================================
# scripts/verify-kernel-services.sh — post-install smoke checks for kernel IdP/mail
# =============================================================================
# Sourced from install-lib.sh. Used by install.sh after Suze (Keycloak) and
# install_stage1_mail (Dovecot when MAIL_SERVICE_MODE=kernel).
#
# Set VERIFY_KERNEL_SERVICES=0 to skip (e.g. air-gapped partial installs).
# =============================================================================

[[ -n "${GENTIAN_VERIFY_KERNEL_SERVICES_LOADED:-}" ]] && return 0
GENTIAN_VERIFY_KERNEL_SERVICES_LOADED=1

_verify_kernel_services_enabled() {
    [[ "${VERIFY_KERNEL_SERVICES:-1}" == "1" ]]
}

_keycloak_service_candidates() {
    local ns="${1:-platform-kernel}"
    local release="${GENTIAN_IDP_KEYCLOAK_RELEASE:-gentian-idp-keycloak}"
    printf '%s\n' \
        "${release}-keycloakx-http" \
        "$(kubectl get svc -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
            | grep -E 'keycloak.*http' | head -1)" \
        "$(kubectl get svc -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
            | grep keycloak | grep -v headless | head -1)"
}

_resolve_keycloak_base_url() {
    local ns="${1:-platform-kernel}"
    local svc port
    while IFS= read -r svc; do
        [[ -z "${svc}" ]] && continue
        kubectl get svc -n "${ns}" "${svc}" >/dev/null 2>&1 || continue
        port=$(kubectl get svc -n "${ns}" "${svc}" \
            -o jsonpath='{.spec.ports[?(@.port==8080)].port}' 2>/dev/null || true)
        port="${port:-8080}"
        echo "http://${svc}.${ns}.svc.cluster.local:${port}/auth"
        return 0
    done < <(_keycloak_service_candidates "${ns}")
    return 1
}

# Run a one-shot Pod; delete it afterward. Returns the container exit code.
_run_ephemeral_pod() {
    local image="$1"
    shift
    local name="gentian-verify-$$-${RANDOM}"
    local phase exit_code=1

    kubectl run "${name}" --restart=Never --image="${image}" \
        --command -- "$@" >/dev/null 2>&1 || return 1

    if ! kubectl wait --for=condition=Ready "pod/${name}" --timeout=120s >/dev/null 2>&1; then
        warn "  Ephemeral pod ${name} did not become Ready."
        kubectl logs "${name}" 2>/dev/null || true
        kubectl delete pod "${name}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
        return 1
    fi

    local deadline=$((SECONDS + 120))
    while (( SECONDS < deadline )); do
        phase=$(kubectl get pod "${name}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        case "${phase}" in
            Succeeded)
                exit_code=0
                break
                ;;
            Failed)
                warn "  Ephemeral pod ${name} failed:"
                kubectl logs "${name}" 2>/dev/null || true
                break
                ;;
        esac
        sleep 2
    done

    kubectl delete pod "${name}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    return "${exit_code}"
}

_verify_http_from_cluster() {
    local url="$1"
    _run_ephemeral_pod "curlimages/curl:8.5.0" \
        curl -sf --max-time 25 "${url}"
}

_verify_tcp_from_cluster() {
    local host="$1"
    local port="$2"
    _run_ephemeral_pod "busybox:1.36" \
        sh -c "nc -z -w 5 ${host} ${port}"
}

# Wait for Keycloak pods, then fetch OIDC discovery for the master realm in-cluster.
verify_keycloak_installation() {
    if ! _verify_kernel_services_enabled; then
        info "Skipping Keycloak verification (VERIFY_KERNEL_SERVICES=0)."
        return 0
    fi

    banner "Verify Keycloak deployment"

    local ns="platform-kernel"
    local timeout="${KEYCLOAK_VERIFY_TIMEOUT:-300}"
    local base_url realm_url

    info "Waiting for Keycloak workload in ${ns} (up to ${timeout}s)..."
    if ! kubectl wait pods -n "${ns}" \
        -l 'app.kubernetes.io/name=keycloak' \
        --for=condition=Ready --timeout="${timeout}s" >/dev/null 2>&1; then
        # keycloakx chart label may differ on older releases — fall back to service endpoints.
        warn "No Ready pod with app.kubernetes.io/name=keycloak — checking Service endpoints..."
        local kc_svc deadline=$((SECONDS + timeout))
        kc_svc="$(_keycloak_service_candidates "${ns}" | head -1)"
        while (( SECONDS < deadline )); do
            if kubectl get endpoints -n "${ns}" "${kc_svc}" \
                -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q .; then
                break
            fi
            sleep 5
        done
    fi

    if ! base_url=$(_resolve_keycloak_base_url "${ns}"); then
        error "Keycloak HTTP Service not found in ${ns}."
        error "  kubectl get svc,pods -n ${ns} | grep -i keycloak"
        return 1
    fi

    realm_url="${base_url}/realms/master/.well-known/openid-configuration"
    info "Probing Keycloak OIDC discovery: ${realm_url}"

    local deadline=$((SECONDS + timeout)) ok=0
    while (( SECONDS < deadline )); do
        if _verify_http_from_cluster "${realm_url}"; then
            ok=1
            break
        fi
        info "  …Keycloak not answering yet (${SECONDS}s/${deadline}s)"
        sleep 10
    done

    if [[ "${ok}" -ne 1 ]]; then
        error "Keycloak OIDC discovery did not return HTTP 200 within ${timeout}s."
        error "  kubectl logs -n ${ns} -l app.kubernetes.io/name=keycloak --tail=40"
        return 1
    fi

    success "Keycloak is live (master realm OIDC discovery OK)."
    return 0
}

# When MAIL_SERVICE_MODE=kernel, confirm Dovecot Deployment, Service endpoints, and IMAP/LMTP ports.
verify_dovecot_installation() {
    if ! _verify_kernel_services_enabled; then
        info "Skipping Dovecot verification (VERIFY_KERNEL_SERVICES=0)."
        return 0
    fi

    local mode="${MAIL_SERVICE_MODE:-external}"
    if [[ "${mode}" != "kernel" ]]; then
        info "Skipping Dovecot verification (MAIL_SERVICE_MODE=${mode})."
        return 0
    fi

    banner "Verify Dovecot deployment"

    local env="${ENV:-dev}"
    local ns="gentian-${env}"
    local deploy="dovecot-${env}"
    local svc="dovecot-${env}"
    local timeout="${DOVECOT_VERIFY_TIMEOUT:-300}"

    info "Waiting for Deployment/${deploy} in ${ns} (up to ${timeout}s)..."
    if ! kubectl wait "deployment/${deploy}" -n "${ns}" \
        --for=condition=Available --timeout="${timeout}s" >/dev/null 2>&1; then
        error "Dovecot Deployment ${deploy} is not Available."
        error "  kubectl describe deployment/${deploy} -n ${ns}"
        error "  kubectl logs -n ${ns} -l app.kubernetes.io/name=dovecot --tail=40"
        return 1
    fi

    if ! kubectl get endpoints -n "${ns}" "${svc}" \
        -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q .; then
        error "Dovecot Service ${svc} has no ready endpoints."
        return 1
    fi

    local fqdn="${svc}.${ns}.svc.cluster.local"
    for port in 143 24; do
        info "Probing Dovecot TCP ${fqdn}:${port} from cluster..."
        if ! _verify_tcp_from_cluster "${fqdn}" "${port}"; then
            error "Dovecot TCP check failed on port ${port}."
            error "  kubectl get pods,svc -n ${ns} -l app.kubernetes.io/name=dovecot"
            return 1
        fi
        success "  Dovecot port ${port} accepts connections."
    done

    success "Dovecot is live (Deployment Available, IMAP + LMTP ports open)."
    return 0
}
