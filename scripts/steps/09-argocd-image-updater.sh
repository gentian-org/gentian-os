#!/usr/bin/env bash
# step: 09-argocd-image-updater
# requires: 08-argocd
# provides: ArgoCD Image Updater
# mutates: namespace argocd-image-updater
# pins: argocd

# Its own namespace, not argocd — the chart is installed with
# --namespace argocd-image-updater --create-namespace and only *watches* argocd.

check() {
    kubectl get deployment argocd-image-updater-controller \
        -n argocd-image-updater >/dev/null 2>&1
}

apply() {
    install_argocd_image_updater
}

destroy() {
    if helm status argocd-image-updater -n argocd-image-updater >/dev/null 2>&1; then
        gentian_run helm uninstall argocd-image-updater -n argocd-image-updater || true
    fi
    kubectl delete namespace argocd-image-updater --ignore-not-found=true 2>/dev/null || true
}
