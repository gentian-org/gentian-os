#!/usr/bin/env bash
# step: A-05-cert-manager
# phase: control-plane
# requires: A-03-namespaces
# provides: cert-manager controller and its CRDs
# mutates: namespace cert-manager, cert-manager CRDs
# pins: cert-manager

check() {
    # --no-cluster-infra: someone else owns cert-manager, so its presence or
    # absence is not this installer's verdict to give.
    [[ "${INSTALL_CLUSTER_INFRA:-1}" == "1" ]] || return "${CHECK_UNDEFINED}"
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
    _delete_namespace cert-manager

    # CRDs are cluster-scoped and survive the namespace. The step's `mutates:`
    # names them; leaving them behind means the next install adopts CRDs from a
    # version it did not pin.
    _delete_crds_matching 'cert-manager\.io$' 'cert-manager CRDs'
}
