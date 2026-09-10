#!/usr/bin/env bash
# step: C-05-kernel-services-configmap
# phase: platform
# requires: B-08-cluster-xr
# provides: ConfigMap gentian-kernel-services in platform-kernel
# mutates: one ConfigMap

# Breaks a real ordering deadlock: the IdP mounts KERNEL_DOMAIN from this
# ConfigMap, but the operator chart that renders it does not arrive until
# D-01-operator. Seeded here with the Helm ownership annotations so 26 adopts it
# rather than failing with "invalid ownership metadata".
#
# The IdP itself now arrives through the gentian-claims ApplicationSet, so this
# step no longer names it.

check() {
    kubectl get configmap gentian-kernel-services -n platform-kernel >/dev/null 2>&1
}

apply() {
    ensure_kernel_services_configmap
}

destroy() {
    kubectl delete configmap gentian-kernel-services -n platform-kernel \
        --ignore-not-found=true 2>/dev/null || true
}
