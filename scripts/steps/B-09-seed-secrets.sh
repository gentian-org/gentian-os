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
}
