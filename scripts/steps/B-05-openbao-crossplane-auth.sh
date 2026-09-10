#!/usr/bin/env bash
# step: B-05-openbao-crossplane-auth
# phase: secrets
# requires: B-04-openbao-init
# provides: OpenBao Kubernetes auth backend, crossplane-write policy, audit device
# mutates: OpenBao auth backends and policies

check() {
    # Asked of Kubernetes, not of OpenBao. `bao policy read` needs both a token
    # and VAULT_ADDR, and the driver sets neither before this runs — so the
    # probe answered "cannot tell" on every pass, which must not become a skip:
    # this step mints the credential provider-vault connects with, and skipping
    # it leaves every SecretV2 and auth backend in the Cluster XR unable to
    # sync, with the error pointing at a Secret no step admits to owning.
    #
    # The Secret is this step's most consequential output and is visible without
    # any OpenBao access at all, which also makes --status honest about it.
    kubectl get secret openbao-crossplane-token \
        -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" >/dev/null 2>&1
}

apply() {
    bootstrap_openbao_for_crossplane
}

destroy() {
    # OpenBao's own storage is removed with its PVCs in 12/13 teardown.
    return 0
}
