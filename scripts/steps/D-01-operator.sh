#!/usr/bin/env bash
# step: D-01-operator
# phase: applications
# requires: B-07-cluster-xr
# provides: gentian-os operator with the authz bridge
# mutates: namespace gentian-system, gentianos.io CRDs

check() {
    kubectl get deployment -n gentian-system \
        -l app.kubernetes.io/name=gentian-os -o name 2>/dev/null | grep -q .
}

apply() {
    install_gentian_os_operator
}

destroy() {
    if helm status gentian-os -n gentian-system >/dev/null 2>&1; then
        gentian_run helm uninstall gentian-os -n gentian-system || true
    fi
    _delete_gentianos_api_scaffold || true
}
