#!/usr/bin/env bash
# step: 05-cluster-issuers
# requires: 04-cert-manager
# provides: ClusterIssuers for kernel certificates
# mutates: cluster-scoped ClusterIssuer objects

check() {
    kubectl get clusterissuer >/dev/null 2>&1 || return 1
    [[ -n "$(kubectl get clusterissuer -o name 2>/dev/null)" ]]
}

apply() {
    install_kernel_cert_resources
}

destroy() {
    kubectl delete clusterissuer --all --ignore-not-found=true 2>/dev/null || true
}
