#!/usr/bin/env bash
# =============================================================================
# scripts/lib/verify-kernel-services.sh — post-install smoke check for kernel mail
# =============================================================================
# Sourced from scripts/lib/load.sh. One verifier remains: Dovecot, when the
# cluster runs its own mail. No install step calls it; its caller is
# crossplane/tests/e2e/scripts/e2e-verify-kernel-services.sh
# (make verify-kernel-services), against a cluster that is already installed.
#
# There were three. verify_keycloak_installation, verify_keycloak_iframe_policy
# and verify_argocd_controller were written and never wired to a caller, so they
# reported nothing on any install; they are deleted rather than left looking like
# coverage. Anything worth checking here should be called from the step that owns
# the thing being checked, the way mail-lib.sh calls the one below.
#
# Set VERIFY_KERNEL_SERVICES=0 to skip (e.g. air-gapped partial installs).
# =============================================================================

[[ -n "${GENTIAN_VERIFY_KERNEL_SERVICES_LOADED:-}" ]] && return 0
GENTIAN_VERIFY_KERNEL_SERVICES_LOADED=1

_verify_kernel_services_enabled() {
    [[ "${VERIFY_KERNEL_SERVICES:-1}" == "1" ]]
}

# Run a one-shot Pod in the given namespace; delete it afterward. Returns the
# container exit code.
#
# The namespace is the caller's to say, and it is the one of the server being
# probed: a server's NetworkPolicy admits a probe from beside it, by the
# gentianos.io/purpose=verify label below, and from nowhere else. The pod used
# to run in `default`, which no server's policy admits. VERIFY_POD_NAMESPACE
# still overrides it.
_run_ephemeral_pod() {
    local ns="${VERIFY_POD_NAMESPACE:-$1}"
    local image="$2"
    shift 2
    local name="gentian-verify-$$-${RANDOM}"
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

_verify_tcp_from_cluster() {
    local ns="$1"
    local host="$2"
    local port="$3"
    _run_ephemeral_pod "${ns}" "busybox:1.36" \
        sh -c "nc -z -w 5 ${host} ${port}"
}

# When MAIL_SERVICE_MODE=system, confirm Dovecot Deployment, Service endpoints, and IMAP/LMTP ports.
verify_dovecot_installation() {
    if ! _verify_kernel_services_enabled; then
        info "Skipping Dovecot verification (VERIFY_KERNEL_SERVICES=0)."
        return 0
    fi

    local mode

    mode="$(gentian_mail_service_mode)"
    if [[ "${mode}" != "system" ]]; then
        info "Skipping Dovecot verification (MAIL_SERVICE_MODE=${mode})."
        return 0
    fi

    banner "Verify Dovecot deployment"

    local env="${ENV:-dev}"
    # The system tier's mail namespace (kernel/namespaces.yaml), where the
    # mail ApplicationSet deploys Dovecot.
    local ns
    ns="$(ns_system mail)" || return 1
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
        if ! _verify_tcp_from_cluster "${ns}" "${fqdn}" "${port}"; then
            error "Dovecot TCP check failed on port ${port}."
            error "  kubectl get pods,svc -n ${ns} -l app.kubernetes.io/name=dovecot"
            return 1
        fi
        success "  Dovecot port ${port} accepts connections."
    done

    success "Dovecot is live (Deployment Available, IMAP + LMTP ports open)."
    return 0
}
