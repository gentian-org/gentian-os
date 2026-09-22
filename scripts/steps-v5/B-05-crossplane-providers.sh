#!/usr/bin/env bash
# step: B-05-crossplane-providers
# phase: secrets
# requires: B-04-vault-auth
# provides: Crossplane providers kubernetes, helm and vault with their ProviderConfigs, in the provisioning namespace
# mutates: Provider packages, cluster-scoped RBAC for the providers, ProviderConfigs

# The provider manifests in crossplane/providers/ name v4's namespaces and the
# vault's v4 address. They are applied here with the layout's names substituted
# rather than duplicated: one set of manifests, two layouts, until v4 goes.

_v5_providers_apply() {
    local file="$1" prov secrets
    prov="$(ns_kernel provisioning)"; secrets="$(ns_kernel secrets)"
    sed -e "s/namespace: crossplane-system/namespace: ${prov}/g" \
        -e "s/serviceaccount:crossplane-system:/serviceaccount:${prov}:/g" \
        -e "s#https://openbao\.openbao\.svc\.cluster\.local:8200#https://openbao.${secrets}.svc.cluster.local:8200#g" \
        "${SCRIPT_DIR}/crossplane/providers/${file}" | _kubectl_retry apply -f -
}

check() {
    local p pc
    for p in provider-kubernetes provider-helm provider-vault; do
        [[ "$(kubectl get provider.pkg.crossplane.io "${p}" -o jsonpath='{.status.conditions[?(@.type=="Healthy")].status}' 2>/dev/null)" == "True" ]] || return 1
    done
    for pc in providerconfig.kubernetes.crossplane.io/kubernetes providerconfig.helm.crossplane.io/kubernetes providerconfig.vault.upbound.io/openbao; do
        kubectl get "${pc}" >/dev/null 2>&1 || return 1
    done
    return 0
}

apply() {
    banner "Crossplane providers"
    export CROSSPLANE_NAMESPACE
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    _v5_providers_apply providers.yaml
    local p
    for p in provider-kubernetes provider-helm provider-vault; do
        info "waiting for ${p} to be Healthy"
        kubectl wait provider.pkg.crossplane.io/"${p}" --for=condition=Healthy --timeout=600s >/dev/null
    done
    _v5_providers_apply provider-rbac.yaml
    _v5_providers_apply provider-configs.yaml
}

destroy() {
    kubectl delete -f "${SCRIPT_DIR}/crossplane/providers/provider-configs.yaml" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete -f "${SCRIPT_DIR}/crossplane/providers/providers.yaml" --ignore-not-found >/dev/null 2>&1 || true
}
