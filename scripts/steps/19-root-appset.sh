#!/usr/bin/env bash
# step: 19-root-appset
# requires: 16-cluster-xr
# provides: root ApplicationSet — the app-of-apps everything else arrives through
# mutates: ApplicationSet gentian-appsets in argocd

check() {
    kubectl get application gentian-appsets -n argocd >/dev/null 2>&1
}

apply() {
    bootstrap_root_appset
}

destroy() {
    kubectl delete application gentian-appsets -n argocd \
        --ignore-not-found=true 2>/dev/null || true
}
