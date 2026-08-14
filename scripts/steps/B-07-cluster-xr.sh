#!/usr/bin/env bash
# step: B-07-cluster-xr
# phase: secrets
# requires: B-06-crossplane-secrets
# provides: Cluster claim — KV mount, policies, AppProject, ClusterSecretStore
# mutates: Cluster claim in crossplane-system and everything it composes

check() {
    local claim
    claim="$(gentian_cluster_claim_name 2>/dev/null || true)"
    [[ -n "$claim" ]] || return 1
    kubectl get cluster.gentianos.io "$claim" -n crossplane-system \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null |
        grep -q True
}

apply() {
    apply_cluster_xr
}

destroy() {
    # Deleting the claim is the whole teardown: Crossplane garbage-collects
    # every managed resource the composition created.
    local claim xr
    claim="$(gentian_cluster_claim_name 2>/dev/null || true)"
    if [[ -n "$claim" ]]; then
        gentian_run kubectl delete cluster.gentianos.io "$claim" \
            -n crossplane-system --ignore-not-found=true --timeout=120s || true
    fi
    for xr in $(kubectl get xcluster.gentianos.io -o name 2>/dev/null || true); do
        kubectl delete "$xr" --ignore-not-found=true --timeout=60s 2>/dev/null || true
    done
}
