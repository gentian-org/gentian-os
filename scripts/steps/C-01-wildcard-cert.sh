#!/usr/bin/env bash
# step: C-01-wildcard-cert
# phase: platform
# requires: B-08-cluster-xr
# provides: wildcard TLS certificate for *.${KERNEL_DOMAIN} (optional, DNS-01 only)
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

    # Absent — and with no DNS provider there is nothing to issue it with, so
    # this is not applicable rather than incomplete.
    #
    # Asked of the cluster's configuration, not of one provider's variable.
    # Gating on CF_API_TOKEN meant a cluster on Route 53 reported its wildcard
    # step undefined forever, and a cluster that had moved off Cloudflare kept
    # reporting it applicable because the old variable was still exported.
    [[ "$(gentian_dns_provider)" != "none" ]] || return "${CHECK_UNDEFINED}"

    # Whether the credential has actually been supplied is deliberately NOT
    # asked here. It lives in OpenBao, and a check() that reads OpenBao needs a
    # token the driver does not hold on the --status path — so it would answer
    # "cannot tell" and be read as "not applicable". apply() asks, because it
    # has the token, and says which credential is missing.
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
