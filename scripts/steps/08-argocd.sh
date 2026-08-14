#!/usr/bin/env bash
# step: 08-argocd
# requires: 02-namespaces
# provides: ArgoCD server and controllers
# mutates: namespace argocd, ArgoCD CRDs
# pins: argocd

check() {
    kubectl get deployment argocd-server -n argocd >/dev/null 2>&1 &&
        kubectl get crd applications.argoproj.io >/dev/null 2>&1
}

apply() {
    install_argocd
}

destroy() {
    # Applications carry finalizers that block namespace deletion when their
    # controller is already gone, so strip them before uninstalling the chart.
    _argocd_strip_kubectl || true
    _argocd_strip_raw || true
    if helm status argocd -n argocd >/dev/null 2>&1; then
        gentian_run helm uninstall argocd -n argocd || true
    fi
    kubectl delete namespace argocd --ignore-not-found=true 2>/dev/null || true
}
