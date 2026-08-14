#!/usr/bin/env bash
# step: 10-openbao-transit
# requires: 08-argocd
# provides: transit-seal OpenBao Application (auto-unseal root, no cloud KMS)
# mutates: ArgoCD Application openbao-transit, namespace openbao

check() {
    kubectl get application openbao-transit -n argocd >/dev/null 2>&1 &&
        kubectl get statefulset openbao-transit -n openbao >/dev/null 2>&1
}

apply() {
    bootstrap_transit_app
}

destroy() {
    kubectl delete application openbao-transit -n argocd \
        --ignore-not-found=true 2>/dev/null || true
}
