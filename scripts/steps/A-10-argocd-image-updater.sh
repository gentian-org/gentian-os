#!/usr/bin/env bash
# step: A-10-argocd-image-updater
# phase: control-plane
# requires: A-09-argocd
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

    # The ImageUpdater CR lives in the argocd namespace, not this step's own, so
    # deleting only argocd-image-updater leaves it behind. Its finalizer is
    # cleared by the controller the uninstall above has just removed, so it then
    # holds the argocd namespace in Terminating indefinitely — a failure that
    # surfaces one step later, in A-09, as a namespace delete that never returns.
    _argocd_strip_kubectl imageupdaters.argocd-image-updater.argoproj.io || true
    kubectl delete imageupdaters.argocd-image-updater.argoproj.io --all \
        -n argocd --ignore-not-found=true 2>/dev/null || true

    _delete_namespace argocd-image-updater
}
