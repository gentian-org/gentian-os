#!/usr/bin/env bash
# step: B-05-crossplane-providers
# phase: secrets
# requires: B-04-vault-auth
# provides: the activation policy naming the provider resource types the platform uses, from this checkout, and those types as established CRDs; Crossplane providers kubernetes, helm and vault with their ProviderConfigs, in the provisioning namespace; provider-helm's second ProviderConfig tenant-apps, acting as the narrow account tenant apps are installed as
# mutates: the ManagedResourceActivationPolicy gentian-platform, Provider packages, cluster-scoped RBAC for the providers, the ServiceAccount provider-helm-tenant-apps and the Secret provider-helm-tenant-apps (a kubeconfig holding no credential) in the provisioning namespace, ProviderConfigs

# The CRDs the three providers install, which their ProviderConfigs are
# instances of. Named rather than discovered: a list that is written down is
# one a reader can check against provider-configs.yaml.
_V5_PROVIDER_CRDS=(
    "providerconfigs.kubernetes.crossplane.io"
    "providerconfigs.helm.crossplane.io"
    "providerconfigs.vault.upbound.io"
)

# Which provider resource types exist: crossplane/providers/activation.yaml.
#
# A-04 installs Crossplane without the policy that activates every type, so a
# provider that declares safe-start (vault, keycloak) gets only the types this
# policy names. It is applied before the providers, so that a provider's types
# are activated as its package installs them rather than a reconcile later.
_B05_ACTIVATION_FILE="${SCRIPT_DIR}/crossplane/providers/activation.yaml"
_B05_ACTIVATION_KIND="managedresourceactivationpolicies.apiextensions.crossplane.io"

_b05_activation_names() {
    _gentian_yq 'select(.kind == "ManagedResourceActivationPolicy") | .metadata.name' "${_B05_ACTIVATION_FILE}"
}

# The types the policy names that have to turn up as CRDs. A wildcard entry
# names no single CRD and is not waited for.
_b05_activated_types() {
    _gentian_yq 'select(.kind == "ManagedResourceActivationPolicy") | .spec.activate[]' "${_B05_ACTIVATION_FILE}" \
        | grep -v '[*?[]' || true
}

_b05_activation_sha() { sha256_of "${_B05_ACTIVATION_FILE}" | cut -c1-16; }

# Existence is not enough, for the reason B-06 gives for Compositions: a list
# that grew in the checkout and was not re-applied leaves the new type off
# while the step reports satisfied. The applied file's checksum is recorded on
# the policy and compared.
_b05_activation_current() {
    local name
    for name in $(_b05_activation_names); do
        [[ "$(kubectl get "${_B05_ACTIVATION_KIND}" "${name}" -o jsonpath='{.metadata.annotations.gentianos\.io/source-sha}' 2>/dev/null)" == "$(_b05_activation_sha)" ]] || return 1
    done
    return 0
}

# The provider roles, compared the same way. A kind added to the roles in the
# checkout (provider-kubernetes-kinds.yaml) was otherwise never applied: the
# step found the providers healthy and skipped, and the API server went on
# refusing every object of that kind. The checksum is recorded on the role
# provider-kubernetes is bound to.
_B05_RBAC_FILE="${SCRIPT_DIR}/crossplane/providers/provider-rbac.yaml"
_B05_RBAC_ROLE="gentian-provider-kubernetes"
_b05_rbac_sha() { sha256_of "${_B05_RBAC_FILE}" | cut -c1-16; }

_b05_rbac_current() {
    [[ "$(kubectl get clusterrole "${_B05_RBAC_ROLE}" -o jsonpath='{.metadata.annotations.gentianos\.io/source-sha}' 2>/dev/null)" == "$(_b05_rbac_sha)" ]]
}

_b05_crd_established() {
    [[ "$(kubectl get crd "$1" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null)" == "True" ]]
}

# _b05_wait_activated_types -- every type the policy names, as an established
# CRD, or a failure that says which is missing and what to do.
#
# Polled rather than `kubectl wait`: the CRD does not exist until Crossplane
# has installed the provider's package and acted on the policy, and `kubectl
# wait` on a name that does not exist yet fails at once. Without this wait a
# missing type shows up much later and somewhere else -- as a composite that
# never becomes Ready.
_b05_wait_activated_types() {
    local deadline missing crd
    deadline=$((SECONDS + ${GENTIAN_ACTIVATION_TIMEOUT:-600}))
    info "waiting for the activated provider resource types to be Established"
    while :; do
        missing=""
        for crd in $(_b05_activated_types); do
            _b05_crd_established "${crd}" || missing+="${crd} "
        done
        [[ -z "${missing}" ]] && return 0
        (( SECONDS < deadline )) || break
        sleep 5
    done
    error "These provider resource types are in crossplane/providers/activation.yaml but did not appear as CRDs:"
    for crd in ${missing}; do error "    ${crd}"; done
    error "  See what Crossplane holds:  kubectl get mrd | grep -v Active ; kubectl get mrap ; kubectl get providers"
    error "  A provider that is not Healthy has not installed its types yet -- run the installer again."
    error "  To install every type of every provider instead, as before: CROSSPLANE_ACTIVATE_ALL=true in install.env,"
    error "  then run the installer again."
    return 1
}

# Providers this platform installed once and no longer does. providers.yaml is
# applied, not synced, so a provider that left the file would stay on a cluster
# installed before it left -- with its CRDs, and through every purge.
_B05_RETIRED_PROVIDERS=(
    "provider-http"
)

_b05_remove_retired_providers() {
    local p
    for p in "${_B05_RETIRED_PROVIDERS[@]}"; do
        kubectl delete provider.pkg.crossplane.io "${p}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
}

# The functions declared in crossplane/providers/providers.yaml, read from the
# file so the wait cannot fall behind what is installed.
_v5_function_names() {
    _gentian_yq 'select(.kind == "Function") | .metadata.name' \
        "${SCRIPT_DIR}/crossplane/providers/providers.yaml"
}

# The provider manifests name the provisioning namespace and the vault's
# address as kernel/namespaces.yaml has them.
_v5_providers_apply() {
    _kubectl_retry apply -f - < "${SCRIPT_DIR}/crossplane/providers/${1:?file}"
}

# The kubeconfig of provider-helm's ProviderConfig tenant-apps
# (provider-configs.yaml): the provider pod's own token, and the API server
# asked to act as provider-helm-tenant-apps, which holds rights only inside
# tenant namespaces (provider-rbac.yaml). It holds no credential: the token
# file is the pod's own and the cluster's CA is public. The CA is given as
# data because the provider reads no file a kubeconfig names but the token.
_B05_TENANT_APPS="provider-helm-tenant-apps"

_b05_tenant_apps_kubeconfig() {
    local ns ca
    ns="$(ns_kernel provisioning)"
    ca="$(kubectl get configmap kube-root-ca.crt -n "${ns}" -o jsonpath='{.data.ca\.crt}' 2>/dev/null)" || return 1
    [[ -n "${ca}" ]] || return 1
    printf '%s\n' \
        "apiVersion: v1" \
        "kind: Config" \
        "clusters:" \
        "- name: in-cluster" \
        "  cluster:" \
        "    server: https://kubernetes.default.svc" \
        "    certificate-authority-data: $(printf '%s\n' "${ca}" | base64 | tr -d '\n')" \
        "users:" \
        "- name: tenant-apps" \
        "  user:" \
        "    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token" \
        "    as: system:serviceaccount:${ns}:${_B05_TENANT_APPS}" \
        "contexts:" \
        "- name: tenant-apps" \
        "  context:" \
        "    cluster: in-cluster" \
        "    user: tenant-apps" \
        "current-context: tenant-apps"
}

# Whether the Secret holds the kubeconfig as this cluster's CA has it now.
_b05_tenant_apps_current() {
    local want have
    want="$(_b05_tenant_apps_kubeconfig)" || return 1
    have="$(kubectl get secret "${_B05_TENANT_APPS}" -n "$(ns_kernel provisioning)" \
        -o jsonpath='{.data.kubeconfig}' 2>/dev/null | base64 -d 2>/dev/null)" || return 1
    [[ "${have}" == "${want}" ]]
}

_b05_tenant_apps_apply() {
    local kubeconfig
    if ! kubeconfig="$(_b05_tenant_apps_kubeconfig)"; then
        error "No kube-root-ca.crt in $(ns_kernel provisioning): the tenant-apps kubeconfig cannot be written."
        return 1
    fi
    printf '%s\n' "${kubeconfig}" |
        kubectl create secret generic "${_B05_TENANT_APPS}" -n "$(ns_kernel provisioning)" \
            --from-file=kubeconfig=/dev/stdin --dry-run=client -o yaml |
        _kubectl_retry apply -f - >/dev/null
}

# _b05_packages_current -- every provider and function the checkout declares
# runs the package it declares. The health checks below say the packages
# are up, not which they are: a pin bumped in providers.yaml was otherwise
# never applied to a cluster that already had healthy providers, and the
# cluster stayed on the old package for good (docs/plans/versioning.md).
# Read with kubectl's client-side dry run, so no YAML tool is needed.
_b05_packages_current() {
    local declared line kind name want have
    declared="$(kubectl apply --dry-run=client -o json -f "${SCRIPT_DIR}/crossplane/providers/providers.yaml" 2>/dev/null \
        | jq -r '(if .kind == "List" then .items[] else . end) | select(.kind == "Provider" or .kind == "Function") | [.kind, .metadata.name, .spec.package] | @tsv')" || return 1
    [[ -n "${declared}" ]] || return 1
    while IFS=$'\t' read -r kind name want; do
        [[ -n "${name}" ]] || continue
        have="$(kubectl get "$(to_lower "${kind}").pkg.crossplane.io" "${name}" -o jsonpath='{.spec.package}' 2>/dev/null)" || return 1
        [[ "${have}" == "${want}" ]] || return 1
    done <<< "${declared}"
    return 0
}

check() {
    local p pc fn crd
    _b05_activation_current || return 1
    _b05_packages_current || return 1
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
    for crd in $(_b05_activated_types); do
        _b05_crd_established "${crd}" || return 1
    done
    for pc in providerconfig.kubernetes.crossplane.io/kubernetes providerconfig.helm.crossplane.io/kubernetes providerconfig.helm.crossplane.io/tenant-apps providerconfig.vault.upbound.io/openbao; do
        kubectl get "${pc}" >/dev/null 2>&1 || return 1
    done
    # The narrow account tenant apps are installed as, and the kubeconfig
    # that makes provider-helm act as it.
    kubectl get serviceaccount "${_B05_TENANT_APPS}" -n "$(ns_kernel provisioning)" >/dev/null 2>&1 || return 1
    _b05_tenant_apps_current || return 1
    _b05_rbac_current || return 1
    return 0
}

apply() {
    banner "Crossplane providers"
    export CROSSPLANE_NAMESPACE
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    # A provider CRD a previous teardown left Terminating is owned by a
    # revision that no longer exists, and the revision this install creates
    # cannot take control of it -- the provider then never becomes healthy.
    _b05_release_terminating_provider_crds
    local p fn crd name
    _kubectl_retry apply -f "${_B05_ACTIVATION_FILE}"
    for name in $(_b05_activation_names); do
        kubectl annotate "${_B05_ACTIVATION_KIND}" "${name}" --overwrite "gentianos.io/source-sha=$(_b05_activation_sha)" >/dev/null
    done
    _v5_providers_apply providers.yaml
    _b05_remove_retired_providers
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
    _b05_wait_activated_types || return 1
    _v5_providers_apply provider-rbac.yaml
    kubectl annotate clusterrole "${_B05_RBAC_ROLE}" --overwrite "gentianos.io/source-sha=$(_b05_rbac_sha)" >/dev/null
    _b05_tenant_apps_apply || return 1
    _v5_providers_apply provider-configs.yaml
    # The functions the Compositions call, before B-06 applies any of them.
    # A Composition whose function is not installed yet renders nothing, and
    # nothing downstream says so.
    for fn in $(_v5_function_names); do
        info "waiting for ${fn} to be Healthy"
        kubectl wait function.pkg.crossplane.io/"${fn}" --for=condition=Healthy --timeout=600s >/dev/null
    done
}

# The managed resources a provider still holds, swept before its ProviderConfig.
#
# A ProviderConfig in use carries the in-use finalizer and refuses deletion
# while any managed resource names it, so `kubectl delete` of the configs hung
# for ever on the first purge that found one. They outlive the steps that made
# them: the operator creates a Release per Component, and by the time this
# step runs the Gentian kinds that owned them are gone, so nothing upstream
# deletes them. Worse, provider-helm reinstalls a Release whose namespace the
# purge already removed, recreating that namespace.
#
# So: ask each provider to delete what it manages while it still runs, give
# it a bounded wait, and strip the finalizers of whatever is left -- this is a
# purge, and the namespaces those releases installed into go with it anyway.
_b05_sweep_managed() {
    local left deadline obj ns
    # Every managed resource of every provider -- `managed` is the category
    # Crossplane gives them all. Naming kinds missed the Keycloak provider's.
    kubectl get managed -o name 2>/dev/null | xargs_r kubectl delete --wait=false >/dev/null 2>&1 || true
    deadline=$((SECONDS + 90))
    while (( SECONDS < deadline )); do
        left="$(kubectl get managed -o name 2>/dev/null || true)"
        [[ -n "${left}" ]] || return 0
        sleep 5
    done
    while IFS= read -r obj; do
        [[ -n "${obj}" ]] || continue
        warn "  ${obj} was not released by its provider; removing its finalizer."
        # A Release's target namespace, so one the provider recreated
        # does not outlive the purge.
        ns="$(kubectl get "${obj}" -o jsonpath='{.spec.forProvider.namespace}' 2>/dev/null || true)"
        kubectl patch "${obj}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
        [[ -z "${ns}" ]] || kubectl delete namespace "${ns}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done < <(kubectl get managed -o name 2>/dev/null || true)
}

# _b05_release_provider_configs -- every ProviderConfig of every provider,
# whoever applied it.
#
# A ProviderConfig carries in-use.crossplane.io while usages name it, and only
# its provider clears that. The Keycloak one is applied by the platform's
# compositions, not by provider-configs.yaml, so it was never deleted here:
# it outlived its provider, held its CRD in Terminating, and the next install's
# provider-keycloak could not take control of that CRD and never became
# healthy -- the Cluster composite then waited 15 minutes on a kind nothing
# served. So: delete them all while the providers still run, wait, strip what
# is left.
_b05_release_provider_configs() {
    local crds crd deadline left obj
    crds="$(kubectl get crd -o name 2>/dev/null | sed -n 's|^customresourcedefinition.apiextensions.k8s.io/\(providerconfigs\..*\)$|\1|p' || true)"
    [[ -n "${crds}" ]] || return 0
    for crd in ${crds}; do
        kubectl get "${crd}" -A -o name 2>/dev/null | xargs_r kubectl delete --wait=false >/dev/null 2>&1 || true
    done
    deadline=$((SECONDS + 60))
    while (( SECONDS < deadline )); do
        left=""
        for crd in ${crds}; do left+="$(kubectl get "${crd}" -A -o name 2>/dev/null || true)"; done
        [[ -n "${left}" ]] || return 0
        sleep 5
    done
    for crd in ${crds}; do
        while IFS= read -r obj; do
            [[ -n "${obj}" ]] || continue
            warn "  ${obj} kept its in-use finalizer; removing it."
            kubectl patch "${obj}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
        done < <(kubectl get "${crd}" -o name 2>/dev/null || true)
    done
}

# _b05_release_terminating_provider_crds -- after the providers are gone, a
# provider CRD still Terminating is held by an object whose finalizer nobody
# will clear any more. Clearing it lets the CRD go, so the next install's
# ProviderRevision can create and own it instead of meeting one owned by a
# revision that no longer exists.
_b05_release_terminating_provider_crds() {
    local crd obj
    while IFS= read -r crd; do
        [[ -n "${crd}" ]] || continue
        while IFS= read -r obj; do
            [[ -n "${obj}" ]] || continue
            kubectl patch "${obj}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
        done < <(kubectl get "${crd}" -A -o name 2>/dev/null || true)
        warn "  ${crd} was left Terminating; released what held it."
    done < <(kubectl get crd -o json 2>/dev/null | jq -r '.items[]
        | select(.metadata.deletionTimestamp != null)
        | select(any(.metadata.ownerReferences[]?; .kind == "ProviderRevision"))
        | .metadata.name' 2>/dev/null || true)
}

destroy() {
    _b05_sweep_managed
    _b05_release_provider_configs
    # Bounded, so a usage nobody can clear is a warning and not a hung purge.
    kubectl delete -f "${SCRIPT_DIR}/crossplane/providers/provider-configs.yaml" --ignore-not-found --timeout=120s >/dev/null 2>&1 \
        || warn "  Some ProviderConfigs did not delete within 2 minutes; the purge continues."
    kubectl delete -f "${SCRIPT_DIR}/crossplane/providers/providers.yaml" --ignore-not-found --timeout=180s >/dev/null 2>&1 || true
    kubectl delete secret "${_B05_TENANT_APPS}" -n "$(ns_kernel provisioning)" --ignore-not-found >/dev/null 2>&1 || true
    _b05_remove_retired_providers
    _b05_release_terminating_provider_crds
    # Last: the policy goes after the providers whose types it activated.
    kubectl delete -f "${_B05_ACTIVATION_FILE}" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
}
