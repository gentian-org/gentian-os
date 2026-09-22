#!/usr/bin/env bash
# step: A-02-cert-manager
# phase: control-plane
# requires: A-01-namespaces
# provides: cert-manager controller and its CRDs in the edge namespace
# mutates: the edge namespace, cert-manager CRDs, cluster-scoped RBAC
# pins: cert-manager

check() {
    helm_pinned_ok cert-manager cert-manager edge &&
        kubectl get crd certificates.cert-manager.io >/dev/null 2>&1
}

apply() {
    banner "cert-manager"
    # DNS-01 challenges are checked against public resolvers, not the cluster's
    # own DNS, which under a hairpinned kernel domain never sees the record.
    helm_pinned cert-manager cert-manager edge \
        --set crds.enabled=true \
        --set 'extraArgs={--dns01-recursive-nameservers-only,--dns01-recursive-nameservers=1.1.1.1:53\,9.9.9.9:53}'
}

destroy() {
    # Shared cluster infrastructure: removing it discards every certificate,
    # and Let's Encrypt allows five per identifier set per week.
    if [[ "${GENTIAN_PURGE_CLUSTER_INFRA:-0}" != "1" ]]; then
        info "Keeping cert-manager (shared cluster infrastructure; --cluster-infra removes it)."
        return 0
    fi
    helm uninstall cert-manager -n "$(ns_kernel edge)" >/dev/null 2>&1 || true
}
