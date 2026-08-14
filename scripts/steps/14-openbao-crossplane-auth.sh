#!/usr/bin/env bash
# step: 14-openbao-crossplane-auth
# phase: secrets
# requires: 13-openbao-init
# provides: OpenBao Kubernetes auth backend, crossplane-write policy, audit device
# mutates: OpenBao auth backends and policies

check() {
    # Needs an authenticated OpenBao; when we cannot talk to it, report unmet
    # and let apply() produce the real diagnostic rather than guessing here.
    [[ -n "${BAO_TOKEN:-}" ]] || return 1
    bao policy read crossplane-write >/dev/null 2>&1
}

apply() {
    bootstrap_openbao_for_crossplane
}

destroy() {
    # OpenBao's own storage is removed with its PVCs in 12/13 teardown.
    return 0
}
