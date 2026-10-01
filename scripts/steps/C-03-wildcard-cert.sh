#!/usr/bin/env bash
# step: C-03-wildcard-cert
# phase: platform
# requires: C-02-appsets
# provides: the cluster issuers and the wildcard certificate for *.<kernelDomain>, copied into the namespaces that terminate TLS for a kernel host
# mutates: ClusterIssuers, the Certificate in cert-manager, and wildcard-tls Secrets in the edge and gitops namespaces; on this host, the kept copy of the wildcard under ~/.gentian/certs

# The Gateway's listener reads wildcard-tls from its own namespace: without it
# the listener is invalid and the Gateway is never programmed, whatever else
# is right.

_v5_wildcard_targets() { echo "$(ns_kernel edge) $(ns_kernel gitops)"; }

check() {
    # The claim says which provider hosts the zone, not the environment: a
    # --status pass carries no DNS_PROVIDER and would otherwise report a
    # cluster that is serving the certificate as undefined.
    [[ "$(gentian_dns_provider)" == "none" ]] && return "${CHECK_UNDEFINED}"
    # Issued by the issuer the claim names now. A switch from staging to
    # production otherwise left the staging certificate in place for good:
    # it was still there and still propagated, which was all this asked.
    [[ "$(kubectl get certificate wildcard-kernel -n "$(gentian_cert_manager_namespace)" \
        -o jsonpath='{.spec.issuerRef.name}' 2>/dev/null)" == "$(gentian_dns01_cluster_issuer_name)" ]] \
        || return "${CHECK_MISSING}"
    GENTIAN_WILDCARD_TARGETS="$(_v5_wildcard_targets)" kernel_wildcard_propagated
}

apply() {
    export SERVICES_NAMESPACE GENTIAN_WILDCARD_TARGETS OPENBAO_NAMESPACE
    SERVICES_NAMESPACE="$(ns_kernel edge)"
    GENTIAN_WILDCARD_TARGETS="$(_v5_wildcard_targets)"
    OPENBAO_NAMESPACE="$(ns_kernel secrets)"
    install_kernel_wildcard
}

destroy() {
    local ns
    # Before the namespace that holds it goes: the next install reuses it.
    save_kernel_wildcard
    for ns in $(_v5_wildcard_targets); do
        kubectl delete secret wildcard-tls -n "${ns}" --ignore-not-found >/dev/null 2>&1 || true
    done
}
