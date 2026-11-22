#!/usr/bin/env bash
# step: B-04-vault-auth
# phase: secrets
# requires: B-03-vault-init
# provides: the vault's KV mount, the crossplane-write policy, the ServiceAccount crossplane-provider-vault in the provisioning namespace, and the vault's Kubernetes auth with the role that account signs in as
# mutates: vault secrets engines, auth backends, policies and roles; ServiceAccount crossplane-provider-vault in the provisioning namespace

# Crossplane's provider-vault
# is what configures every other role and policy from the Cluster claim, so
# this step lets its account sign in for that and no more. It leaves no token
# behind: the provider gets a short-lived one each time it signs in.

check() {
    vault_provider_in_sync
}

apply() {
    export OPENBAO_NAMESPACE CROSSPLANE_NAMESPACE GENTIAN_SYSTEM_NAMESPACE
    OPENBAO_NAMESPACE="$(ns_kernel secrets)"
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    # init_openbao is idempotent and is what puts BAO_TOKEN in the environment.
    init_openbao
    bootstrap_openbao_for_crossplane
}

destroy() {
    kubectl delete serviceaccount crossplane-provider-vault -n "$(ns_kernel provisioning)" --ignore-not-found >/dev/null 2>&1 || true
}
