#!/usr/bin/env bash
# step: D-01-operator
# phase: applications
# requires: B-08-cluster-xr
# provides: gentian-os operator with the authz bridge
# mutates: namespace gentian-system, gentianos.io CRDs

check() {
    kubectl get deployment -n gentian-system \
        -l app.kubernetes.io/name=gentian-os -o name 2>/dev/null | grep -q . || return 1

    # The API scaffold, not just the Deployment. _delete_gentianos_api_scaffold
    # (this step's own destroy()) deletes every gentianos.io CRD and the
    # validating webhook; the Deployment can stay Running with both gone since
    # neither is on its own pod's readiness path. tenants.gentianos.io stands
    # in for the CRD set — C-06, D-08 and E-01 all break silently downstream
    # without it, the same shape as A-02's missing ProviderConfig.
    kubectl get crd tenants.gentianos.io >/dev/null 2>&1 || return 1
    kubectl get validatingwebhookconfiguration gentian-os-tenant-validator >/dev/null 2>&1 || return 1

    _authz_bridge_has_credential
}

# _authz_bridge_has_credential — this step provides "the operator with the authz
# bridge", so the bridge holding a credential is part of what it provides.
#
# The Deployment reads OPENFGA_API_TOKEN with optional: true, deliberately: the
# operator should start while the ExternalSecret is still syncing and pick the
# token up on the restart its reloader annotation triggers. The cost of that
# tolerance is that a Secret which will NEVER arrive — an unwritten OpenBao path,
# an ExternalSecret stuck in SecretSyncedError — looks exactly like one still on
# its way. Kubernetes swallows it, the operator runs, and the bridge writes no
# tuples while every object involved reports healthy.
#
# A literal token in the Helm values used to paper over that, which is why
# removing it means testing for the real thing instead. Read from the Deployment
# rather than from Helm values or the claim: the env var is what the operator
# actually resolves, and a bridge disabled for this cluster renders no env var at
# all — the same evidence, read the other way.
_authz_bridge_has_credential() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}" ref name key

    ref="$(kubectl get deployment -n "${ns}" -l app.kubernetes.io/name=gentian-os \
        -o jsonpath='{range .items[0].spec.template.spec.containers[0].env[?(@.name=="OPENFGA_API_TOKEN")]}{.valueFrom.secretKeyRef.name}{" "}{.valueFrom.secretKeyRef.key}{end}' \
        2>/dev/null || true)"

    # No env var: authzBridge.enabled is false for this cluster. Nothing to hold.
    [[ -n "${ref}" ]] || return 0

    name="${ref%% *}"
    key="${ref##* }"
    [[ -n "${name}" && -n "${key}" ]] || return 0

    kubectl get secret "${name}" -n "${ns}" \
        -o jsonpath="{.data.${key}}" 2>/dev/null | grep -q .
}

apply() {
    install_gentian_os_operator
}

destroy() {
    if helm status gentian-os -n gentian-system >/dev/null 2>&1; then
        gentian_run helm uninstall gentian-os -n gentian-system || true
    fi

    # The workload, whether or not a local Helm release owned it.
    #
    # On a cluster that has reconciled, the operator carries
    # argocd.argoproj.io/tracking-id: Argo CD renders the same chart and owns
    # the objects, so there is no local release for the uninstall above to find
    # and it silently does nothing. The Deployment then survives a teardown that
    # reports success — and check(), which asks only whether the Deployment
    # exists, reports the step satisfied on a torn-down cluster.
    kubectl delete deployment,service,replicaset -n gentian-system \
        -l app.kubernetes.io/name=gentian-os \
        --ignore-not-found=true --wait=false 2>/dev/null || true

    _delete_gentianos_api_scaffold || true
}
