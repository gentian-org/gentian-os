#!/usr/bin/env bash
# step: A-03-namespaces
# phase: control-plane
# requires: A-01-crossplane
# provides: kernel namespaces (openbao, external-secrets, argocd, gentian-system, platform-kernel, gentian-infra-<stage>)
# mutates: namespaces only

_kernel_namespaces() {
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    echo "openbao external-secrets argocd gentian-system platform-kernel ${INFRA_NAMESPACE:-gentian-infra-${stage}}"
}

check() {
    local ns
    for ns in $(_kernel_namespaces); do
        kubectl get namespace "$ns" >/dev/null 2>&1 || return 1
    done
    return 0
}

apply() {
    create_namespaces
}

destroy() {
    # Last step to run in reverse order, so everything inside is already gone.
    local ns
    for ns in $(_kernel_namespaces); do
        _delete_namespace "$ns" || true
    done
}
