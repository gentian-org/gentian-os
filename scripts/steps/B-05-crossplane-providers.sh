#!/usr/bin/env bash
# step: B-05-crossplane-providers
# phase: secrets
# requires: B-04-vault-auth
# provides: Crossplane providers kubernetes, helm and vault with their ProviderConfigs, in the provisioning namespace
# mutates: Provider packages, cluster-scoped RBAC for the providers, ProviderConfigs

# The provider manifests in crossplane/providers/ name v4's namespaces and the
# vault's v4 address. They are applied here with the layout's names substituted
# rather than duplicated: one set of manifests, two layouts, until v4 goes.

# The CRDs the three providers install, which their ProviderConfigs are
# instances of. Named rather than discovered: a list that is written down is
# one a reader can check against provider-configs.yaml.
_V5_PROVIDER_CRDS=(
    "providerconfigs.kubernetes.crossplane.io"
    "providerconfigs.helm.crossplane.io"
    "providerconfigs.vault.upbound.io"
)

# The functions declared in crossplane/providers/providers.yaml, read from the
# file so the wait cannot fall behind what is installed.
_v5_function_names() {
    _gentian_yq 'select(.kind == "Function") | .metadata.name' \
        "${SCRIPT_DIR}/crossplane/providers/providers.yaml"
}

_v5_providers_apply() {
    local file="$1" prov secrets
    prov="$(ns_kernel provisioning)"; secrets="$(ns_kernel secrets)"
    sed -e "s/namespace: crossplane-system/namespace: ${prov}/g" \
        -e "s/serviceaccount:crossplane-system:/serviceaccount:${prov}:/g" \
        -e "s#https://openbao\.openbao\.svc\.cluster\.local:8200#https://openbao.${secrets}.svc.cluster.local:8200#g" \
        "${SCRIPT_DIR}/crossplane/providers/${file}" | _kubectl_retry apply -f -
}

check() {
    local p pc fn crd
    for p in provider-kubernetes provider-helm provider-vault; do
        [[ "$(kubectl get provider.pkg.crossplane.io "${p}" -o jsonpath='{.status.conditions[?(@.type=="Healthy")].status}' 2>/dev/null)" == "True" ]] || return 1
    done
    # The functions every Composition calls. A Composition applied while one
    # of them is still installing renders nothing, and B-06 records a
    # source-sha for it anyway -- so a step that never waited would report
    # satisfied over a cluster composing from nothing.
    for fn in $(_v5_function_names); do
        [[ "$(kubectl get function.pkg.crossplane.io "${fn}" -o jsonpath='{.status.conditions[?(@.type=="Healthy")].status}' 2>/dev/null)" == "True" ]] || return 1
    done
    for crd in "${_V5_PROVIDER_CRDS[@]}"; do
        [[ "$(kubectl get crd "${crd}" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null)" == "True" ]] || return 1
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
    local p fn crd
    for p in provider-kubernetes provider-helm provider-vault; do
        info "waiting for ${p} to be Healthy"
        kubectl wait provider.pkg.crossplane.io/"${p}" --for=condition=Healthy --timeout=600s >/dev/null
    done
    # Healthy is not Established.
    #
    # A Provider reports Healthy when its pod runs; it creates its own CRDs
    # shortly afterwards. Applying a ProviderConfig in that window fails with
    # "no matches for kind ProviderConfig", and _kubectl_retry does not retry
    # that -- it retries connection errors only. The v4 installer waits here
    # for exactly this reason, and the comment there records what it cost:
    # a Keycloak Release stuck on a ProviderConfig that was never created.
    for crd in "${_V5_PROVIDER_CRDS[@]}"; do
        info "waiting for ${crd} to be Established"
        kubectl wait crd "${crd}" --for=condition=Established --timeout=300s >/dev/null
    done
    _v5_providers_apply provider-rbac.yaml
    _v5_providers_apply provider-configs.yaml
    # The functions the Compositions call, before B-06 applies any of them.
    # A Composition whose function is not installed yet renders nothing, and
    # nothing downstream says so.
    for fn in $(_v5_function_names); do
        info "waiting for ${fn} to be Healthy"
        kubectl wait function.pkg.crossplane.io/"${fn}" --for=condition=Healthy --timeout=600s >/dev/null
    done
}

destroy() {
    kubectl delete -f "${SCRIPT_DIR}/crossplane/providers/provider-configs.yaml" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete -f "${SCRIPT_DIR}/crossplane/providers/providers.yaml" --ignore-not-found >/dev/null 2>&1 || true
}
