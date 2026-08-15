#!/usr/bin/env bash
# step: B-09-seed-secrets
# phase: secrets
# requires: B-07-cluster-xr
# provides: remaining OpenBao paths (registry, DNS, mail)
# mutates: OpenBao KV paths only

check() {
    # kv_put_once makes seeding idempotent, so re-running is safe and cheap;
    # probing every path here would duplicate that logic without adding safety.
    return 1
}

apply() {
    seed_secrets_remaining

    # OpenBao now holds every credential the cache was standing in for, and
    # try_load_creds_from_openbao recovers them from here on, so the local copy
    # is redundant — and a redundant credential on disk is just a credential on
    # disk.
    clear_credential_cache
}
