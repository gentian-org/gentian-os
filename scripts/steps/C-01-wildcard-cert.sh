#!/usr/bin/env bash
# step: C-01-wildcard-cert
# phase: platform
# requires: B-07-cluster-xr
# provides: wildcard TLS certificate for *.${KERNEL_DOMAIN} (optional)
# mutates: Certificate in cert-manager, wildcard-tls Secrets in kernel namespaces

check() {
    # The cluster is asked first, and the token only decides what an ABSENT
    # certificate means.
    #
    # Gating on CF_API_TOKEN up front asked a question this shell cannot answer:
    # the token is collected for an install and unset on --status, so a cluster
    # serving the certificate reported undefined. Whether the certificate exists
    # is a property of the cluster, not of the environment reading it.
    if kubectl get secret wildcard-kernel-tls -n cert-manager >/dev/null 2>&1; then
        # Both halves, because `mutates:` names both. cert-manager holds the
        # issued certificate; the Gateway reads the copy in the services
        # namespace, and a certificate issued but never distributed leaves the
        # edge serving nothing while the issuing half looks complete.
        kubectl get secret wildcard-tls -n "$(gentian_services_namespace)" >/dev/null 2>&1 ||
            return "${CHECK_MISSING}"
        return 0
    fi

    # Absent — and with no DNS-01 token there is nothing to issue it with, so
    # this is not applicable rather than incomplete.
    [[ -n "${CF_API_TOKEN:-}" ]] || return "${CHECK_UNDEFINED}"
    return "${CHECK_MISSING}"
}

apply() {
    install_kernel_wildcard
}

destroy() {
    kubectl delete certificate wildcard-kernel-tls -n cert-manager \
        --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret wildcard-kernel-tls -n cert-manager \
        --ignore-not-found=true 2>/dev/null || true
}
