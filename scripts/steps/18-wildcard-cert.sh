#!/usr/bin/env bash
# step: 18-wildcard-cert
# requires: 16-cluster-xr
# provides: wildcard TLS certificate for *.${KERNEL_DOMAIN} (optional)
# mutates: Certificate in cert-manager, wildcard-tls Secrets in kernel namespaces

check() {
    # Optional step: without a DNS-01 token there is nothing to issue.
    [[ -n "${CF_API_TOKEN:-}" ]] || return 0
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
