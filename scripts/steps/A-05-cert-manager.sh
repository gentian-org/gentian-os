#!/usr/bin/env bash
# step: A-05-cert-manager
# phase: control-plane
# requires: A-03-namespaces
# provides: cert-manager controller and its CRDs
# mutates: namespace cert-manager, cert-manager CRDs
# pins: cert-manager

check() {
    [[ "${INSTALL_CLUSTER_INFRA:-1}" == "1" ]] || return 0
    kubectl get deployment cert-manager -n cert-manager >/dev/null 2>&1 &&
        kubectl get crd certificates.cert-manager.io >/dev/null 2>&1
}

apply() {
    install_cert_manager
}

destroy() {
    if helm status cert-manager -n cert-manager >/dev/null 2>&1; then
        gentian_run helm uninstall cert-manager -n cert-manager || true
    fi
    kubectl delete namespace cert-manager --ignore-not-found=true 2>/dev/null || true
}
