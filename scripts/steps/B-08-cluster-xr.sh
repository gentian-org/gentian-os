#!/usr/bin/env bash
# step: B-08-cluster-xr
# phase: secrets
# requires: B-07-openbao-oidc-mount
# provides: Cluster claim — KV mount, policies, AppProject, ClusterSecretStore
# mutates: Cluster claim in crossplane-system and everything it composes

check() {
    local claim
    claim="$(gentian_cluster_claim_name 2>/dev/null || true)"
    # No claim name means the deployments checkout could not be read, so there
    # is nothing to look the XR up by — a different answer from "the XR is not
    # Ready", and reporting the latter blames the cluster for a local gap.
    [[ -n "$claim" ]] || return "${CHECK_UNDEFINED}"

    # The same question apply() waited on, not a stricter one.
    #
    # The claim's own Ready aggregates every composed resource without
    # exception, and three of them — the Keycloak Client and its two mappers —
    # cannot reconcile until the root ApplicationSet delivers Keycloak at
    # sync-wave 9 and its ProviderConfig at 16, both applied by C-02, four
    # steps after this one. wait_for_xcluster_ready already accounts for that
    # and returns once everything this step OWES is ready.
    #
    # Asking the aggregate here meant check() contradicted apply(): the step
    # ran, succeeded, and then reported itself unsatisfied for work it does not
    # do and cannot influence. The driver's end-of-run report then named B-08
    # as outstanding at the end of a complete install, and the advice it gave —
    # run install.sh again — changed nothing, because there was nothing here
    # left to run. It cleared later, on its own, once the Keycloak objects
    # reconciled, which is the definition of not this step's business.
    local xr
    xr="$(kubectl get cluster.gentianos.io "$claim" -n crossplane-system \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)"
    [[ -n "$xr" ]] || return "${CHECK_MISSING}"
    xcluster_structural_ready "$xr" || return "${CHECK_MISSING}"

    # Ready is not the same as current: this step is the claim's only applier
    # (the claims ApplicationSet excludes it), so a claim edited in Git and
    # never applied would otherwise report satisfied.
    cluster_claim_is_current "$claim"
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
