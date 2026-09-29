#!/usr/bin/env bash
# step: B-02-seal-init
# phase: secrets
# requires: B-01-bootstrap-apps
# provides: initialised transit seal in the seal namespace, its unseal key as a Secret there, and the transit token the vault reads from the secrets namespace
# mutates: transit OpenBao data; Secrets openbao-transit-unseal (seal namespace), openbao-transit-token (seal and secrets namespaces); ~/.gentian/openbao-transit-init.json

# The seal and the vault are two namespaces on purpose: the seal's unseal key
# stays in kernel-seal, a separate RBAC and backup domain, and the only thing
# that crosses to kernel-secrets is the token the vault uses to ask the seal
# to unseal it. That copy is what this step adds to the v4 script's work.

_seal_ns()  { ns_kernel seal; }
_vault_ns() { ns_kernel secrets; }

_transit_token_valid_in() {
    local ns="$1" token addr
    token="$(kubectl get secret openbao-transit-token -n "${ns}" -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null)" || return 1
    [[ -n "${token}" ]] || return 1
    addr="$(gentian_service_addr openbao-transit "$(_seal_ns)" 8200 http 2>/dev/null)" || return 1
    [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "X-Vault-Token: ${token}" "${addr}/v1/auth/token/lookup-self")" == "200" ]]
}

check() {
    kubectl get secret openbao-transit-unseal -n "$(_seal_ns)" >/dev/null 2>&1 &&
        _transit_token_valid_in "$(_seal_ns)" &&
        _transit_token_valid_in "$(_vault_ns)" &&
        [[ "$(kubectl get application openbao-transit -n "$(ns_kernel gitops)" -o jsonpath='{.status.health.status}' 2>/dev/null)" == "Healthy" ]]
}

apply() {
    banner "Transit seal"
    export TRANSIT_NAMESPACE
    TRANSIT_NAMESPACE="$(_seal_ns)"
    init_openbao_transit
    # The vault reads the token from its own namespace.
    kubectl get secret openbao-transit-token -n "${TRANSIT_NAMESPACE}" -o json \
        | jq 'del(.metadata.namespace, .metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.managedFields, .metadata.ownerReferences)' \
        | kubectl apply -n "$(_vault_ns)" -f - >/dev/null
    success "openbao-transit-token copied to $(_vault_ns) for the vault."
}

destroy() {
    kubectl delete secret openbao-transit-token -n "$(_vault_ns)" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete secret openbao-transit-token openbao-transit-unseal -n "$(_seal_ns)" --ignore-not-found >/dev/null 2>&1 || true
}
