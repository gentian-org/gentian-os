#!/usr/bin/env bash
# step: A-03-external-secrets
# phase: control-plane
# requires: A-01-namespaces
# provides: External Secrets Operator and its CRDs in the secrets namespace
# mutates: the secrets namespace, ESO CRDs, cluster-scoped RBAC
# pins: external-secrets

check() {
    helm_pinned_ok external-secrets external-secrets secrets &&
        kubectl get crd externalsecrets.external-secrets.io >/dev/null 2>&1
}

apply() {
    banner "External Secrets Operator"
    helm_pinned external-secrets external-secrets secrets --set installCRDs=true
}

destroy() {
    helm uninstall external-secrets -n "$(ns_kernel secrets)" >/dev/null 2>&1 || true
}
