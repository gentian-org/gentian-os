#!/usr/bin/env bash
# step: B-03-vault-init
# phase: secrets
# requires: B-02-seal-init
# provides: initialised, auto-unsealed vault in the secrets namespace; BAO_TOKEN for the rest of the run
# mutates: vault storage; ~/.gentian/openbao-init.json (the recovery key and root token, until the recovery kit holds them)
# pins: openbao

_vault_ns() { ns_kernel secrets; }

check() {
    local addr
    addr="$(OPENBAO_NAMESPACE="$(_vault_ns)" gentian_service_addr openbao "$(_vault_ns)" 8200 https 2>/dev/null)" || return 1
    local st
    st="$(curl -sk --max-time 5 "${addr}/v1/sys/seal-status" 2>/dev/null)" || return 1
    [[ "$(jq -r '.initialized' <<<"${st}")" == "true" && "$(jq -r '.sealed' <<<"${st}")" == "false" ]] &&
        [[ "$(kubectl get application openbao -n "$(ns_kernel gitops)" -o jsonpath='{.status.health.status}' 2>/dev/null)" == "Healthy" ]]
}

apply() {
    banner "Vault"
    export OPENBAO_NAMESPACE GENTIAN_SYSTEM_NAMESPACE
    OPENBAO_NAMESPACE="$(_vault_ns)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    init_openbao
}

destroy() {
    # The vault's data lives in its PVC, which the Application keeps (prune: false).
    # Nothing to undo here that B-01's destroy does not.
    return 0
}
