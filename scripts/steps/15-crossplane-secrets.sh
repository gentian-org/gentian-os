#!/usr/bin/env bash
# step: 15-crossplane-secrets
# phase: secrets
# requires: 14-openbao-crossplane-auth
# provides: derived-credential Secrets consumed by the Cluster XR
# mutates: Secrets in crossplane-system

check() {
    kubectl get secret gentian-os-master-password -n crossplane-system >/dev/null 2>&1
}

apply() {
    create_crossplane_secrets
}

destroy() {
    local secret
    for secret in gentian-os-master-password gentian-registry-credentials \
                  gentian-dns-credentials gentian-smtp-credentials; do
        kubectl delete secret "$secret" -n crossplane-system \
            --ignore-not-found=true 2>/dev/null || true
    done
}
