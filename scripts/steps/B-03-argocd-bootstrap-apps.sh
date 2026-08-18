#!/usr/bin/env bash
# step: B-03-argocd-bootstrap-apps
# phase: secrets
# requires: B-02-openbao-transit-init
# provides: openbao, reloader, cnpg, globals and external-dns Applications
# mutates: ArgoCD Applications in argocd

# OpenBao is deployed BY ArgoCD rather than by this installer, which is what
# keeps it inside drift detection (docs/plans §1, invariant 1).

check() {
    local app
    for app in openbao reloader; do
        kubectl get application "$app" -n argocd >/dev/null 2>&1 || return 1
    done
    return 0
}

apply() {
    bootstrap_argocd_apps
}

destroy() {
    local app
    for app in openbao reloader cnpg cnpg-cluster globals external-dns; do
        kubectl delete application "$app" -n argocd \
            --ignore-not-found=true 2>/dev/null || true
    done
}
