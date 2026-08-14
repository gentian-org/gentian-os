#!/usr/bin/env bash
# step: 17-seed-secrets
# requires: 16-cluster-xr
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
