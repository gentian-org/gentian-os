#!/usr/bin/env bash
# step: B-10-seed-secrets
# phase: secrets
# requires: B-08-cluster-xr
# provides: remaining OpenBao paths (registry, DNS, mail)
# mutates: OpenBao KV paths, plus a force-sync annotation on ExternalSecrets
#          and ClusterIssuers that latched before those paths existed

check() {
    # kv_put_once makes seeding idempotent, so re-running is safe and cheap;
    # probing every path here would duplicate that logic without adding safety.
    return "${CHECK_ALWAYS}"
}

apply() {
    seed_secrets_remaining

    # The paths exist now. The controllers that read them gave up before they
    # did — A-06's ClusterIssuer and B-03's ExternalSecrets were both created
    # several steps ago against a path that answered 403 until this moment —
    # and cert-manager in particular will not re-read an Issuer's solver Secret
    # on its own. Released here, where the seeding they were waiting for
    # happens, rather than left for a later step to trip over.
    resync_credential_consumers

    # OpenBao now holds every credential the cache was standing in for, and
    # try_load_creds_from_openbao recovers them from here on, so the local copy
    # is redundant — and a redundant credential on disk is just a credential on
    # disk.
    clear_credential_cache
}
