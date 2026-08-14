#!/usr/bin/env bash
# step: 22-mac-admission
# requires: 19-root-appset
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
