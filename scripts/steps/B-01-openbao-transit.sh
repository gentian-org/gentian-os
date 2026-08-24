#!/usr/bin/env bash
# step: B-01-openbao-transit
# phase: secrets
# requires: A-09-argocd
# provides: transit-seal OpenBao Application (auto-unseal root, no cloud KMS)
# mutates: ArgoCD Application openbao-transit, namespace openbao

check() {
    kubectl get application openbao-transit -n argocd >/dev/null 2>&1 &&
        kubectl get statefulset openbao-transit -n openbao >/dev/null 2>&1 &&
        # bootstrap_transit_app creates this (as a placeholder, later filled in
        # by B-02) if absent, and B-02's destroy() deletes it — it holds the
        # transit instance's Shamir unseal key, which has no recovery path
        # once lost. Without this check, deleting just the secret (B-02's
        # destroy() run alone, or by hand) leaves the Application/StatefulSet
        # satisfying this function, so apply() — the one thing that recreates
        # it — never runs again.
        kubectl get secret openbao-transit-unseal -n openbao >/dev/null 2>&1
}

apply() {
    bootstrap_transit_app
}

destroy() {
    _delete_argocd_application openbao-transit
}
