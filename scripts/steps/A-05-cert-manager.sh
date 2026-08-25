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
    # Cluster infrastructure, and treated as such: kept unless --cluster-infra.
    #
    # It is the same category as CNPG, Reloader and external-dns — a shared
    # operator this installer brings up, which the flag's own help describes as
    # something that "may serve workloads that are not Gentian's". cert-manager
    # fits that exactly: it issues for whatever asks, and removing it takes every
    # certificate on the machine with it.
    #
    # It was in the plain purge, and that is what made rebuilds expensive.
    # Removing cert-manager discards the wildcard certificate with it, and
    # Let's Encrypt allows five per identifier set per week — so five cycles
    # exhausted the quota and the next install could not obtain one at all.
    # Keeping it means a rebuild reuses the certificate it already has.
    if [[ "${GENTIAN_PURGE_CLUSTER_INFRA:-0}" != "1" ]]; then
        info "Keeping cert-manager and its CRDs (shared cluster infrastructure)."
        info "  Remove them with --cluster-infra."
        return 0
    fi

    if helm status cert-manager -n cert-manager >/dev/null 2>&1; then
        gentian_run helm uninstall cert-manager -n cert-manager || true
    fi
    _delete_namespace cert-manager

    # CRDs are cluster-scoped and survive the namespace. The step's `mutates:`
    # names them; leaving them behind means the next install adopts CRDs from a
    # version it did not pin.
    _delete_crds_matching 'cert-manager\.io$' 'cert-manager CRDs'
}
