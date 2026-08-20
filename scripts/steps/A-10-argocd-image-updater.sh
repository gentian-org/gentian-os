#!/usr/bin/env bash
# step: A-10-argocd-image-updater
# phase: control-plane
# requires: A-09-argocd
# provides: ArgoCD Image Updater
# mutates: namespace argocd-image-updater
# pins: argocd

# Its own namespace, not argocd — the chart is installed with
# --namespace argocd-image-updater --create-namespace and only *watches* argocd.

# Why this is a Helm install and not an Argo CD Application, and why it sits
# here rather than after the root appset:
#
# It is the CD control plane, alongside A-09-argocd. Argo CD cannot deliver the
# thing that bootstraps Argo CD, and its updater companion is bootstrapped the
# same way for the same reason — a broken sync must not be able to remove the
# machinery that ships the fix.
#
# The related and stronger constraint is one step removed, on the Applications
# this controller writes into. gentian-os and gentian-portal both carry
# write-back-method: argocd, so the updater patches image.tag onto the live
# Application object. An Application owned by an ApplicationSet with selfHeal
# would have that patch reverted on the next reconcile and every rollout would
# undo itself — which is why those two are rendered from
# kernel/bootstrap/chart/templates and applied directly, never committed to
# gentian-deployments. See docs/architecture.md §11.1 and deployment.md §3.1.
#
# Nothing later in the sequence requires this step, and that is expected: it is a
# companion, not a dependency. The position is still right — the phase is a label
# rather than an ordering key (69ec9979), and this belongs with Argo CD.

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
