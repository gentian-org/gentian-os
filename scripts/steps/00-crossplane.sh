#!/usr/bin/env bash
# step: 00-crossplane
# requires:
# provides: Crossplane core controller in crossplane-system
# mutates: namespace crossplane-system, Crossplane CRDs, cluster-scoped RBAC
# pins: crossplane

check() {
    kubectl get deployment crossplane -n crossplane-system >/dev/null 2>&1 &&
        kubectl get crd compositeresourcedefinitions.apiextensions.crossplane.io >/dev/null 2>&1
}

apply() {
    install_crossplane
}

destroy() {
    # Order matters: the Helm release first so the controller stops recreating
    # what the following deletes remove, then the cluster-scoped leftovers Helm
    # does not own.
    if helm status crossplane -n crossplane-system >/dev/null 2>&1; then
        gentian_run helm uninstall crossplane -n crossplane-system || true
    fi
    kubectl delete clusterrole \
        crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
        --ignore-not-found=true 2>/dev/null || true
    kubectl delete clusterrolebinding \
        crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
        --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace crossplane-system --ignore-not-found=true 2>/dev/null || true
}
