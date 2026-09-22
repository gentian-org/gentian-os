#!/usr/bin/env bash
# step: B-08-seed-secrets
# phase: secrets
# requires: C-01-cluster-claim
# provides: the kernel KV paths ESO reads — DNS provider, tunnel, mail relay, registry, the deployments repository credential
# mutates: vault KV paths under gentian-os/kernel/

# After C-01: the Cluster claim is what creates the eso-read policy and the
# ClusterSecretStore, so a path seeded earlier would be unreadable until then
# anyway. Values come from the bootstrap credential cache; none is committed.

check() {
    local addr token
    addr="$(gentian_service_addr openbao "$(ns_kernel secrets)" 8200 https 2>/dev/null)" || return 1
    token="$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}" 2>/dev/null)" || return 1
    [[ -n "${token}" ]] || return 1
    local p
    for p in dns/cloudflare repositories/deployments; do
        [[ "$(curl -sk -o /dev/null -w '%{http_code}' --max-time 5 -H "X-Vault-Token: ${token}" "${addr}/v1/secret/data/gentian-os/kernel/${p}")" == "200" ]] || return 1
    done
}

apply() {
    export OPENBAO_NAMESPACE GENTIAN_SYSTEM_NAMESPACE
    OPENBAO_NAMESPACE="$(ns_kernel secrets)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    seed_secrets_remaining
}

destroy() {
    return 0
}
