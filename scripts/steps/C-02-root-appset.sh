#!/usr/bin/env bash
# step: C-02-root-appset
# phase: platform
# requires: B-07-cluster-xr
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
