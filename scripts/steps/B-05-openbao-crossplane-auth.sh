#!/usr/bin/env bash
# step: B-05-openbao-crossplane-auth
# phase: secrets
# requires: B-04-openbao-init
# provides: OpenBao Kubernetes auth backend, crossplane-write policy, audit device
# mutates: OpenBao auth backends and policies

check() {
    # Needs an authenticated OpenBao. On the forward pass B-04 has exported the
    # token by now, so this resolves properly; a read-only pass has no token and
    # genuinely cannot tell, which is not the same as knowing the policy is
    # absent. Saying missing there reports a fault on a healthy cluster.
    [[ -n "${BAO_TOKEN:-}" ]] || return "${CHECK_UNDEFINED}"
    bao policy read crossplane-write >/dev/null 2>&1
}

apply() {
    bootstrap_openbao_for_crossplane
}

destroy() {
    # OpenBao's own storage is removed with its PVCs in 12/13 teardown.
    return 0
}
