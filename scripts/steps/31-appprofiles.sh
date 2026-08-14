#!/usr/bin/env bash
# step: 31-appprofiles
# phase: applications
# requires: 26-operator
# provides: AppProfile CRs from the gentian-apps repository
# mutates: AppProfile objects

check() {
    kubectl get crd appprofiles.gentianos.io >/dev/null 2>&1 &&
        [[ -n "$(kubectl get appprofile -A -o name 2>/dev/null)" ]]
}

apply() {
    bootstrap_appprofiles
}

destroy() {
    kubectl delete appprofile --all -A --ignore-not-found=true 2>/dev/null || true
}
