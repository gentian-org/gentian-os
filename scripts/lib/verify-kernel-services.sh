#!/usr/bin/env bash
# =============================================================================
# scripts/lib/verify-kernel-services.sh — post-install smoke checks for kernel IdP/mail
# =============================================================================
# Sourced from scripts/lib/load.sh. Used by install.sh after Suze (Keycloak) and
# install_kernel_mail (Dovecot when MAIL_SERVICE_MODE=kernel).
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
    local ns="${VERIFY_POD_NAMESPACE:-default}"
    local phase exit_code=1 create_err

    # The pod spec is generated rather than left to `kubectl run`, because a bare
    # `kubectl run` pod is rejected by the platform's OWN Kyverno baseline
    # (kernel/security/kyverno/policies/gentian-baseline.yaml, installed in Step
    # 11c):
    #
    #   admission webhook "validate.kyverno.svc-fail" denied the request:
    #   gentian-disallow-privilege-escalation: Containers must set
    #   allowPrivilegeEscalation to false
    #
    # The verification harness has to satisfy the same baseline it helped
    # install: non-root, no privilege escalation, all capabilities dropped,
    # RuntimeDefault seccomp, no host namespaces or hostPath.
    local manifest
    manifest=$(python3 -c '
import json, sys
name, ns, image = sys.argv[1], sys.argv[2], sys.argv[3]
cmd = sys.argv[4:]
print(json.dumps({
    "apiVersion": "v1", "kind": "Pod",
    "metadata": {"name": name, "namespace": ns,
                 "labels": {"app.kubernetes.io/managed-by": "gentian-os",
                            "gentianos.io/purpose": "verify"}},
    "spec": {
        "restartPolicy": "Never",
        "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                            "seccompProfile": {"type": "RuntimeDefault"}},
        "containers": [{
            "name": "probe", "image": image, "command": cmd,
            "securityContext": {
                "allowPrivilegeEscalation": False, "privileged": False,
                "readOnlyRootFilesystem": True, "runAsNonRoot": True,
                "capabilities": {"drop": ["ALL"]},
                "seccompProfile": {"type": "RuntimeDefault"}},
        }],
    },
}))' "${name}" "${ns}" "${image}" "$@") || return 1

    # Do NOT discard the creation error. Swallowing it is what turned an
    # admission rejection into 300s of "…not answering yet" against a Keycloak
    # that was serving HTTP 200 the whole time.
    if ! create_err=$(printf '%s' "${manifest}" | kubectl apply -f - 2>&1); then
        warn "  Could not create verification pod in namespace ${ns}:"
        printf '    %s\n' "${create_err}" >&2
        return 1
    fi

    # One-shot curl/busybox pods often skip Ready and go straight to Succeeded.
    kubectl wait --for=condition=Ready "pod/${name}" -n "${ns}" --timeout=30s >/dev/null 2>&1 || true

    local deadline=$((SECONDS + 120))
    while (( SECONDS < deadline )); do
        phase=$(kubectl get pod "${name}" -n "${ns}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        case "${phase}" in
            Succeeded)
                exit_code=0
                break
                ;;
            Failed)
                warn "  Ephemeral pod ${name} failed:"
                kubectl logs "${name}" -n "${ns}" 2>/dev/null || true
                break
                ;;
        esac
        sleep 2
    done

    if [[ "${exit_code}" -ne 0 && "${phase}" != "Failed" ]]; then
        warn "  Ephemeral pod ${name} did not complete (phase=${phase:-unknown})."
        kubectl logs "${name}" -n "${ns}" 2>/dev/null || true
    fi

    kubectl delete pod "${name}" -n "${ns}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
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

# =============================================================================
# verify_argocd_controller — is GitOps actually running?
#
# Everything after Step 4 assumes Argo CD reconciles. When the
# application-controller dies, nothing announces it: Applications keep their
# last status, sync just stops happening, and the cluster drifts from git while
# looking healthy. Observed here as a four-hour outage found only by noticing an
# Application pinned to a stale revision — install.sh had already exited 0.
#
# The pre-existing `kubectl rollout status || warn` cannot catch this: it warns
# rather than fails, and it runs at install time, whereas the controller died
# hours later. So check two things that reveal a dead controller regardless of
# when it died:
#
#   1. restart count — a CrashLoopBackOff shows up here long before anything else
#   2. reconciliation freshness — a live controller updates reconciledAt
#      continuously; a stale timestamp across ALL Applications means it is not
#      running, whatever the pod status claims
#
# Advisory by design: it reports loudly and returns non-zero, but callers decide
# whether that is fatal. A degraded Argo CD does not invalidate an otherwise
# successful install — it just must not pass unnoticed.
# =============================================================================
verify_argocd_controller() {
    if ! _verify_kernel_services_enabled; then
        info "Skipping Argo CD controller verification (VERIFY_KERNEL_SERVICES=0)."
        return 0
    fi

    banner "Verify Argo CD controller"

    local pod="argocd-application-controller-0"
    local phase restarts
    phase=$(kubectl get pod "${pod}" -n argocd -o jsonpath='{.status.phase}' 2>/dev/null || true)
    restarts=$(kubectl get pod "${pod}" -n argocd -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)

    if [[ -z "${phase}" ]]; then
        error "Argo CD application-controller not found in namespace argocd."
        error "  Nothing will sync. kubectl get pods -n argocd"
        return 1
    fi

    if [[ "${restarts}" -gt 5 ]]; then
        error "Argo CD application-controller has restarted ${restarts} times."
        error "  Check for OOMKilled: kubectl get pod ${pod} -n argocd -o jsonpath='{.status.containerStatuses[0].lastState}'"
        error "  If OOM: lower ARGOCD_STATUS_PROCESSORS / ARGOCD_OPERATION_PROCESSORS (see tune_argocd_runtime)"
        error "  and consider resource.exclusions in argocd-cm — this cluster has $(kubectl api-resources --verbs=list -o name 2>/dev/null | wc -l) resource types."
        return 1
    fi

    # Freshness: the newest reconciledAt across all Applications. A controller
    # that is running touches at least one of these every few minutes.
    local newest age_min
    newest=$(kubectl get applications -n argocd         -o jsonpath='{range .items[*]}{.status.reconciledAt}{"\n"}{end}' 2>/dev/null | sort -r | head -1)
    if [[ -n "${newest}" ]]; then
        local now_s then_s
        now_s=$(date -u +%s)
        then_s=$(date -u -d "${newest}" +%s 2>/dev/null || echo "${now_s}")
        age_min=$(( (now_s - then_s) / 60 ))
        if (( age_min > 15 )); then
            error "No Application has reconciled for ${age_min} minutes — Argo CD is not syncing."
            error "  kubectl get pod ${pod} -n argocd"
            error "  kubectl logs ${pod} -n argocd --previous --tail=30"
            return 1
        fi
        info "  Most recent reconcile: ${age_min}m ago."
    fi

    success "Argo CD application-controller healthy (phase=${phase}, restarts=${restarts})."
    return 0
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
        -l 'app.kubernetes.io/name=keycloakx' \
        --for=condition=Ready --timeout="${timeout}s" >/dev/null 2>&1 \
        && ! kubectl wait pods -n "${ns}" \
        -l 'app.kubernetes.io/name=keycloak' \
        --for=condition=Ready --timeout=30s >/dev/null 2>&1; then
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
