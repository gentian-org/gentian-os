#!/usr/bin/env bash
# step: C-04-mac-admission
# phase: platform
# requires: C-02-root-appset
# provides: Kyverno admission policies (Stage 0 MAC)
# mutates: Kyverno ClusterPolicy objects

check() {
    kubectl get crd clusterpolicies.kyverno.io >/dev/null 2>&1 &&
        [[ -n "$(kubectl get clusterpolicy -o name 2>/dev/null)" ]]
}

apply() {
    install_mac_admission
}

destroy() {
    _delete_kyverno_scaffold || true
}
