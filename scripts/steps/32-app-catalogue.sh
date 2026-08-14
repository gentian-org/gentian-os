#!/usr/bin/env bash
# step: 32-app-catalogue
# requires: 31-appprofiles
# provides: AppCatalogue CRD and the kubectl-gentian plugin
# mutates: AppCatalogue objects, host CLI plugin

check() {
    kubectl get crd appcatalogues.gentianos.io >/dev/null 2>&1 &&
        [[ -n "$(kubectl get appcatalogue -A -o name 2>/dev/null)" ]]
}

apply() {
    install_app_catalogue
}

destroy() {
    kubectl delete appcatalogue --all -A --ignore-not-found=true 2>/dev/null || true
    _remove_host_cli || true
}
