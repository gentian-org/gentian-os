#!/usr/bin/env bash
# step: A-04-crossplane
# phase: control-plane
# requires: A-01-namespaces
# provides: Crossplane core and its CRDs in the provisioning namespace
# mutates: the provisioning namespace, Crossplane CRDs, cluster-scoped RBAC
# pins: crossplane

check() {
    helm_pinned_ok crossplane crossplane provisioning &&
        kubectl get crd compositeresourcedefinitions.apiextensions.crossplane.io >/dev/null 2>&1
}

apply() {
    banner "Crossplane"
    helm_pinned crossplane crossplane provisioning
}

destroy() {
    helm uninstall crossplane -n "$(ns_kernel provisioning)" >/dev/null 2>&1 || true
}
