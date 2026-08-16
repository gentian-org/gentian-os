#!/usr/bin/env bash
# step: A-09-argocd
# phase: control-plane
# requires: A-03-namespaces
# provides: ArgoCD server and controllers
# mutates: namespace argocd, ArgoCD CRDs
# pins: argocd

check() {
    # Includes the ApplicationSet CRD: the manifest creates Deployments before
    # CRDs, so a run that failed on the CRDs leaves a server that answers this
    # check and a cluster that cannot compose an ApplicationSet.
    argocd_installed
}

apply() {
    install_argocd
}

destroy() {
    # Applications carry finalizers that block namespace deletion when their
    # controller is already gone, so strip them before uninstalling the chart.
    #
    # All three kinds, not just Applications: an ApplicationSet finalizer holds
    # the namespace open just as effectively, and AppProjects are deleted last
    # by the chart uninstall.
    local kind
    for kind in applications applicationsets appprojects; do
        _argocd_strip_kubectl "${kind}.argoproj.io" || true
        _argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/${kind}" || true
    done
    if helm status argocd -n argocd >/dev/null 2>&1; then
        gentian_run helm uninstall argocd -n argocd || true
    fi
    kubectl delete namespace argocd --ignore-not-found=true 2>/dev/null || true
}
