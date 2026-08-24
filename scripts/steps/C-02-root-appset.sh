#!/usr/bin/env bash
# step: C-02-root-appset
# phase: platform
# requires: B-08-cluster-xr
# provides: root ApplicationSet — the app-of-apps everything else arrives through
# mutates: ApplicationSet gentian-appsets in argocd

check() {
    kubectl get application gentian-appsets -n argocd >/dev/null 2>&1 || return "${CHECK_MISSING}"

    # Existence is not enough. Every child ApplicationSet builds its source path
    # from deploymentsCluster, so when that parameter is empty they all point at
    # clusters//kernel/claims — a path that does not exist, which ArgoCD reports
    # as an unremarkable sync failure while the claims it should have applied
    # simply never arrive. Testing only for the object left that unfixable: the
    # step reported satisfied and skipped the apply that would have corrected it.
    local live
    live="$(kubectl get application gentian-appsets -n argocd \
        -o jsonpath='{.spec.source.helm.parameters[?(@.name=="deploymentsCluster")].value}' \
        2>/dev/null || true)"
    [[ "${live}" == "${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}" ]]
}

apply() {
    bootstrap_root_appset
}

destroy() {
    _delete_argocd_application gentian-appsets
}
