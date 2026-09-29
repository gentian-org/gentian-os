#!/usr/bin/env bash
# step: C-03-wildcard-cert
# phase: platform
# requires: C-02-appsets
# provides: the cluster issuers and the wildcard certificate for *.<kernelDomain>, copied into the namespaces that terminate TLS for a kernel host
# mutates: ClusterIssuers, the Certificate in cert-manager, and wildcard-tls Secrets in the edge and gitops namespaces

# The Gateway's listener reads wildcard-tls from its own namespace: without it
# the listener is invalid and the Gateway is never programmed, whatever else
# is right.

_v5_wildcard_targets() { echo "$(ns_kernel edge) $(ns_kernel gitops)"; }

check() {
    # The claim says which provider hosts the zone, not the environment: a
    # --status pass carries no DNS_PROVIDER and would otherwise report a
    # cluster that is serving the certificate as undefined.
    [[ "$(gentian_dns_provider)" == "none" ]] && return "${CHECK_UNDEFINED}"
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
    for ns in $(_v5_wildcard_targets); do
        kubectl delete secret wildcard-tls -n "${ns}" --ignore-not-found >/dev/null 2>&1 || true
    done
}
