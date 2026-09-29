#!/usr/bin/env bash
# step: B-07-crossplane-secrets
# phase: secrets
# requires: B-04-vault-auth
# provides: the derived-credential Secrets the Cluster composition reads (master password and what is derived from it), in the provisioning namespace
# mutates: Secrets in the provisioning namespace; the vault's gentian-os/kernel/internal paths

check() {
    kubectl get secret gentian-os-master-password -n "$(ns_kernel provisioning)" >/dev/null 2>&1
}

apply() {
    export OPENBAO_NAMESPACE CROSSPLANE_NAMESPACE GENTIAN_SYSTEM_NAMESPACE
    OPENBAO_NAMESPACE="$(ns_kernel secrets)"
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    init_openbao
    create_crossplane_secrets
}

destroy() {
    kubectl delete secret gentian-os-master-password -n "$(ns_kernel provisioning)" --ignore-not-found >/dev/null 2>&1 || true
}
