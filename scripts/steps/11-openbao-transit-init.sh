#!/usr/bin/env bash
# step: 11-openbao-transit-init
# requires: 10-openbao-transit
# provides: initialised transit OpenBao + auto-unseal Secret for the primary
# mutates: Secret openbao/openbao-transit-unseal, transit OpenBao data

check() {
    kubectl get secret openbao-transit-unseal -n openbao >/dev/null 2>&1
}

apply() {
    init_openbao_transit
}

destroy() {
    kubectl delete secret openbao-transit-unseal -n openbao \
        --ignore-not-found=true 2>/dev/null || true
}
