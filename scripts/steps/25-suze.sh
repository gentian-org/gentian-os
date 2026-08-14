#!/usr/bin/env bash
# step: 25-suze
# requires: 23-kernel-services-configmap
# provides: Gentian IdP — Keycloak and OpenFGA via the Suze claim
# mutates: Suze claim and the Releases it composes

check() {
    local claim
    claim="$(gentian_suze_claim_name 2>/dev/null || true)"
    [[ -n "$claim" ]] || return 1
    kubectl get suze.gentianos.io "$claim" -n crossplane-system \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null |
        grep -q True
}

apply() {
    apply_suze_xr
}

destroy() {
    local claim xr
    claim="$(gentian_suze_claim_name 2>/dev/null || true)"
    if [[ -n "$claim" ]]; then
        gentian_run kubectl delete suze.gentianos.io "$claim" \
            -n crossplane-system --ignore-not-found=true --timeout=120s || true
    fi
    for xr in $(kubectl get xsuze -o name 2>/dev/null || true); do
        kubectl delete "$xr" --ignore-not-found=true --timeout=60s 2>/dev/null || true
    done
}
