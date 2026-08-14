#!/usr/bin/env bash
# step: C-01-wildcard-cert
# phase: platform
# requires: B-07-cluster-xr
# provides: wildcard TLS certificate for *.${KERNEL_DOMAIN} (optional)
# mutates: Certificate in cert-manager, wildcard-tls Secrets in kernel namespaces

check() {
    # Optional step: without a DNS-01 token there is nothing to issue, so there
    # is also nothing to report as done.
    [[ -n "${CF_API_TOKEN:-}" ]] || return "${CHECK_UNDEFINED}"
    kubectl get secret wildcard-kernel-tls -n cert-manager >/dev/null 2>&1
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
