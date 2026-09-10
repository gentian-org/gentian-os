#!/usr/bin/env bash
# step: A-01-crossplane
# phase: control-plane
# requires:
# provides: Crossplane core controller in crossplane-system, and its own CRDs
# mutates: namespace crossplane-system, Crossplane CRDs, cluster-scoped RBAC
# pins: crossplane

check() {
    kubectl get deployment crossplane -n crossplane-system >/dev/null 2>&1 &&
        kubectl get crd compositeresourcedefinitions.apiextensions.crossplane.io >/dev/null 2>&1
}

apply() {
    install_crossplane

    # Crossplane v2 ships no CRDs in its chart — `helm template` renders zero,
    # and the core binary installs them itself at startup. So the CRDs exist
    # only because a pod once booted, and reinstalling does not necessarily boot
    # one: if the rendered pod spec is unchanged, Kubernetes reuses the existing
    # ReplicaSet and the pod is never replaced.
    #
    # That is how a purge left this cluster with a healthy Crossplane whose own
    # API was missing. check() above spotted the absent XRD CRD and re-applied,
    # helm reported success, and nothing changed: the eight-hour-old pod stayed,
    # so compositions, compositeresourcedefinitions and
    # managedresourcedefinitions never came back. Every provider and function
    # then sat Installed=True Healthy=False with
    #
    #   cannot establish control of object: the server could not find the
    #   requested resource (post managedresourcedefinitions...)
    #
    # and the install blocked in `kubectl wait --for=condition=Healthy`.
    #
    # A restart is the whole remedy, because startup is when they are created.
    local crd missing=0
    for crd in compositions.apiextensions.crossplane.io \
               compositeresourcedefinitions.apiextensions.crossplane.io \
               managedresourcedefinitions.apiextensions.crossplane.io; do
        kubectl get crd "${crd}" >/dev/null 2>&1 || missing=1
    done
    if [[ "${missing}" == "1" ]]; then
        warn "Crossplane core CRDs are missing; restarting it so they are recreated."
        kubectl -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
            rollout restart deploy/crossplane >/dev/null 2>&1 || true
        kubectl -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
            rollout status deploy/crossplane --timeout=180s >/dev/null 2>&1 || true
        for crd in compositions.apiextensions.crossplane.io \
                   compositeresourcedefinitions.apiextensions.crossplane.io \
                   managedresourcedefinitions.apiextensions.crossplane.io; do
            kubectl get crd "${crd}" >/dev/null 2>&1 \
                || error "Crossplane CRD ${crd} still absent after a restart."
        done
        success "Crossplane core CRDs restored."
    fi
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
    # Registered by Crossplane core, cluster-scoped, and left behind by both
    # the helm uninstall and the namespace delete. A stale no-usages webhook
    # rejects deletes across the cluster once its service is gone.
    kubectl delete validatingwebhookconfiguration crossplane-no-usages \
        --ignore-not-found=true --wait=false 2>/dev/null || true

    _delete_namespace crossplane-system
}
