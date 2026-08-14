#!/usr/bin/env bash
# step: A-08-eso
# phase: control-plane
# requires: A-03-namespaces
# provides: External Secrets Operator — the only runtime read path for secrets
# mutates: namespace external-secrets, ESO CRDs
# pins: external-secrets

check() {
    kubectl get deployment external-secrets -n external-secrets >/dev/null 2>&1 &&
        kubectl get crd externalsecrets.external-secrets.io >/dev/null 2>&1
}

apply() {
    install_eso
}

destroy() {
    if helm status external-secrets -n external-secrets >/dev/null 2>&1; then
        gentian_run helm uninstall external-secrets -n external-secrets || true
    fi
    kubectl delete namespace external-secrets --ignore-not-found=true 2>/dev/null || true
}
