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

    # The workload, whether or not a local Helm release owned it.
    #
    # On a cluster that has reconciled, the operator carries
    # argocd.argoproj.io/tracking-id: Argo CD renders the same chart and owns
    # the objects, so there is no local release for the uninstall above to find
    # and it silently does nothing. The Deployment then survives a teardown that
    # reports success — and check(), which asks only whether the Deployment
    # exists, reports the step satisfied on a torn-down cluster.
    kubectl delete deployment,service,replicaset -n gentian-system \
        -l app.kubernetes.io/name=gentian-os \
        --ignore-not-found=true --wait=false 2>/dev/null || true

    _delete_gentianos_api_scaffold || true
}
