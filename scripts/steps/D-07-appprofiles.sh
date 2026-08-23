#!/usr/bin/env bash
# step: D-07-appprofiles
# phase: applications
# requires: D-01-operator
# provides: AppProfile CRs from the gentian-apps repository
# mutates: AppProfile objects

check() {
    kubectl get crd appprofiles.gentianos.io >/dev/null 2>&1 || return "${CHECK_MISSING}"

    # The Repository claim, not "is there at least one AppProfile". This step
    # applies the claim that composes the catalogue-sync ApplicationSet; the
    # profiles arrive afterwards, from Argo CD. A single profile that reached
    # the cluster by some other route satisfied the old test while the
    # catalogue was not syncing at all — which surfaces as a tenant refused
    # admission for an AppProfile that exists in gentian-apps and was never
    # installed here.
    kubectl get repository.gentianos.io gentian-apps -n crossplane-system >/dev/null 2>&1
}

apply() {
    bootstrap_appprofiles
}

destroy() {
    kubectl delete repository.gentianos.io gentian-apps -n crossplane-system --ignore-not-found=true 2>/dev/null || true
    kubectl delete appprofile --all -A --ignore-not-found=true 2>/dev/null || true
}
