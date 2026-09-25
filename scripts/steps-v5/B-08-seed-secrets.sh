#!/usr/bin/env bash
# step: B-08-seed-secrets
# phase: secrets
# requires: B-07-crossplane-secrets
# provides: the kernel KV paths ESO reads — DNS provider, tunnel, mail relay, registry, the deployments repository credential
# mutates: vault KV paths under gentian-os/kernel/

# This said "requires: C-01-cluster-claim", which runs nine steps later. The
# install was never wrong -- the driver reads the line as documentation -- but
# the documentation was, and the next person to reorder the steps would have
# believed it. scripts/lint/lint-step-order.py now refuses a forward
# dependency.
#
# What this actually needs is a vault that is up, unsealed and holding the
# derived credentials, which is B-07 by way of B-04 and B-03. Writing a KV
# path uses the root token and needs no policy. What needs C-01 is READING:
# the Cluster claim creates the eso-read policy and the ClusterSecretStore.
# That is the right way round -- these paths must exist before the claims
# that consume them, or every ExternalSecret starts unsatisfied and waits for
# a refresh.
#
# Values come from the bootstrap credential cache; none is committed.

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

    # The paths exist now. The controllers that read them gave up before they
    # did -- the ClusterIssuer and the bootstrap ExternalSecrets were created
    # several steps ago against a path that answered 403 until this moment --
    # and cert-manager in particular will not re-read an Issuer's solver
    # Secret on its own. Released here, where the seeding they were waiting
    # for happens, rather than left for a later step to trip over. v5 dropped
    # both of these when the step was written and nothing kicked them.
    resync_credential_consumers

    # OpenBao now holds every credential the cache was standing in for, and
    # try_load_creds_from_openbao recovers them from here on, so the local
    # copy is redundant -- and a redundant credential on disk is just a
    # credential on disk.
    clear_credential_cache
}

destroy() {
    return 0
}
