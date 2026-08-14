#!/usr/bin/env bash
# step: 21-infra-data
# requires: 20-provider-helm
# provides: shared PostgreSQL and MariaDB via the InfraData claim
# mutates: InfraData claim and the Releases it composes

check() {
    local claim
    claim="$(gentian_infradata_claim_name 2>/dev/null || true)"
    [[ -n "$claim" ]] || return 1
    kubectl get infradata.gentianos.io "$claim" -n crossplane-system \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null |
        grep -q True
}

apply() {
    apply_infra_data_xr
}

destroy() {
    local claim xr
    claim="$(gentian_infradata_claim_name 2>/dev/null || true)"
    if [[ -n "$claim" ]]; then
        gentian_run kubectl delete infradata.gentianos.io "$claim" \
            -n crossplane-system --ignore-not-found=true --timeout=120s || true
    fi
    for xr in $(kubectl get xinfradata.gentianos.io -o name 2>/dev/null || true); do
        kubectl delete "$xr" --ignore-not-found=true --timeout=60s 2>/dev/null || true
    done
}
