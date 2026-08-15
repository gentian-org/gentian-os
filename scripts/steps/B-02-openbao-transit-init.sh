#!/usr/bin/env bash
# step: B-02-openbao-transit-init
# phase: secrets
# requires: B-01-openbao-transit
# provides: initialised transit OpenBao + auto-unseal Secret for the primary
# mutates: Secrets openbao/openbao-transit-unseal and openbao/openbao-transit-token,
#          transit OpenBao data

check() {
    # B-01 creates openbao-transit-unseal up front, holding the literal string
    # "placeholder", so its existence says nothing about whether transit was
    # ever initialised — testing only for it makes this step satisfied before it
    # has run, forever. The primary then starts with no
    # openbao-transit-token to unseal against and sits in
    # CreateContainerConfigError, blaming a Secret nothing admits to owning.
    kubectl get secret openbao-transit-token -n openbao >/dev/null 2>&1 ||
        return "${CHECK_MISSING}"

    local key
    key="$(kubectl get secret openbao-transit-unseal -n openbao \
        -o jsonpath='{.data.unseal-key}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    [[ -n "${key}" && "${key}" != "placeholder" ]]
}

apply() {
    init_openbao_transit
}

destroy() {
    kubectl delete secret openbao-transit-unseal -n openbao \
        --ignore-not-found=true 2>/dev/null || true
}
