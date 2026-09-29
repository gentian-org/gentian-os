#!/usr/bin/env bash
# step: B-04-vault-auth
# phase: secrets
# requires: B-03-vault-init
# provides: the vault's KV mount, the crossplane-write policy, and the openbao-crossplane-token Secret in the provisioning namespace
# mutates: vault secrets engines, policies and tokens; Secret openbao-crossplane-token in the provisioning namespace

# What v4's B-05 did, addressed through the layout: Crossplane's provider-vault
# is what configures the vault's Kubernetes auth and every role from the
# Cluster claim, so this step gives it a token that may do that and no more.

check() {
    local ns token addr
    ns="$(ns_kernel provisioning)"
    # provider-vault reads the token as JSON under the key "credentials".
    token="$(kubectl get secret openbao-crossplane-token -n "${ns}" -o jsonpath='{.data.credentials}' 2>/dev/null | base64 -d 2>/dev/null | jq -r '.token // empty' 2>/dev/null)" || return 1
    [[ -n "${token}" ]] || return 1
    addr="$(gentian_service_addr openbao "$(ns_kernel secrets)" 8200 https 2>/dev/null)" || return 1
    [[ "$(curl -sk -o /dev/null -w '%{http_code}' --max-time 5 -H "X-Vault-Token: ${token}" "${addr}/v1/auth/token/lookup-self")" == "200" ]]
}

apply() {
    export OPENBAO_NAMESPACE CROSSPLANE_NAMESPACE GENTIAN_SYSTEM_NAMESPACE
    OPENBAO_NAMESPACE="$(ns_kernel secrets)"
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    # init_openbao is idempotent and is what puts BAO_TOKEN in the environment.
    init_openbao
    bootstrap_openbao_for_crossplane
}

destroy() {
    kubectl delete secret openbao-crossplane-token -n "$(ns_kernel provisioning)" --ignore-not-found >/dev/null 2>&1 || true
}
