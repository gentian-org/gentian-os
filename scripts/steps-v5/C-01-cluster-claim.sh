#!/usr/bin/env bash
# step: C-01-cluster-claim
# phase: platform
# requires: B-06-crossplane-definitions
# provides: the Cluster claim of clusters/<cluster>/kernel/claims/cluster.yaml applied and its composite Ready — the vault's Kubernetes auth and roles, the ESO ClusterSecretStore, the kernel config
# mutates: the Cluster claim in the provisioning namespace and everything its composition creates

# The claim comes from the deployments checkout, written by the installer's
# own scaffold (--prepare-deployment) so every cluster's claim has one shape.
# Its spec.layout must be this installer's; apply_cluster_xr refuses otherwise.

check() {
    local claim
    claim="$(gentian_cluster_claim_name 2>/dev/null)" || return 1
    [[ "$(kubectl get cluster.gentianos.io "${claim}" -n "$(ns_kernel provisioning)" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]]
}

apply() {
    export CROSSPLANE_NAMESPACE OPENBAO_NAMESPACE
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    OPENBAO_NAMESPACE="$(ns_kernel secrets)"
    apply_cluster_xr
}

destroy() {
    local claim
    claim="$(gentian_cluster_claim_name 2>/dev/null)" || return 0
    kubectl delete cluster.gentianos.io "${claim}" -n "$(ns_kernel provisioning)" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
