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
    # Kept unless --cluster-infra, because re-obtaining it is rationed.
    #
    # Let's Encrypt allows five certificates per exact identifier set per 168
    # hours. [gtn.host, *.gtn.host] is one set, so five rebuilds in a week is the
    # budget — and a teardown that discards the certificate spends one on every
    # cycle. That limit was reached here, and the next install could not obtain
    # a certificate at all:
    #
    #   429 rateLimited: too many certificates (5) already issued for this exact
    #   set of identifiers in the last 168h0m0s
    #
    # which stops the Gateway serving HTTPS, and with it every check that reads a
    # public URL. Nothing else in the teardown is scarce in that way: a database
    # can be recreated in seconds, a certificate cannot be recreated at all until
    # a clock somewhere else runs out.
    #
    # It is also safe to keep. A public certificate is not a credential — the
    # private key is regenerated on renewal, and a stale one for a domain this
    # cluster no longer serves grants nobody anything.
    if [[ "${GENTIAN_PURGE_CLUSTER_INFRA:-0}" != "1" ]]; then
        info "Keeping the wildcard certificate (Let's Encrypt allows five per week)."
        info "  Remove it with --cluster-infra if the domain is changing."
        return 0
    fi

    # The Certificate is wildcard-kernel; wildcard-kernel-tls is the Secret it
    # writes. Deleting the latter under the former's name removed nothing, so the
    # Certificate outlived the purge, found its Secret gone, and re-issued —
    # spending a quota slot to replace something the teardown had just discarded.
    kubectl delete certificate wildcard-kernel -n cert-manager \
        --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret wildcard-kernel-tls -n cert-manager \
        --ignore-not-found=true 2>/dev/null || true
}
