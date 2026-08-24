#!/usr/bin/env bash
# =============================================================================
# scripts/lib/teardown.sh — teardown primitives
# =============================================================================
# Shared teardown helpers called by step destroy(). They lived in uninstall.sh, which no longer
# exists — uninstall is the driver run in reverse, so the helpers have to be library code.
# =============================================================================

[[ -n "${GENTIAN_TEARDOWN_LOADED:-}" ]] && return 0
GENTIAN_TEARDOWN_LOADED=1

# ProviderConfigs must be deleted individually — kubectl delete -f fails if
# a CRD is not registered (e.g. provider-helm was never installed).
# Add --wait=false so deletion doesn't block if a stale usage reference lingers.
_delete_provider_config() {
    local resource="$1"   # e.g. providerconfig.kubernetes.crossplane.io/kubernetes
    local label="$2"
    if kubectl get "${resource}" >/dev/null 2>&1; then
        # Strip the usage finalizer explicitly first, then delete without waiting.
        kubectl patch "${resource}" \
            --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
            2>/dev/null || true
        kubectl delete "${resource}" --ignore-not-found=true --wait=false 2>/dev/null || true
        success "  ProviderConfig ${label} removed."
    else
        info "  ProviderConfig ${label} not found or CRD absent; skipping."
    fi
}

# Explicitly delete all provider-installed CRDs.  When providers are removed
# (Step 3), Crossplane's package manager normally GCs their CRDs.  But if
# Crossplane core is already gone (re-entrant uninstall), the package manager
# is not running and CRDs become orphaned.  Belt-and-suspenders: always
# sweep these groups after helm uninstall so the next install gets clean CRDs.
_delete_crossplane_crds() {
    local pattern='crossplane\.io|upbound\.io'
    local crds left deadline last_report=0 left_count=0 aggressive_done=0

    crds=$(kubectl get crd -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | grep -E "${pattern}" || true)

    if [[ -z "${crds}" ]]; then
        info "No Crossplane/Upbound CRDs found; skipping CRD sweep."
        return 0
    fi

    info "Deleting Crossplane/Upbound CRDs..."
    while IFS= read -r crd; do
        [[ -z "${crd}" ]] && continue
        kubectl patch crd "${crd}" \
            --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
            2>/dev/null || true
        kubectl delete crd "${crd}" --ignore-not-found=true --wait=false 2>/dev/null || true
    done <<< "${crds}"

    deadline=$((SECONDS + 180))
    while :; do
        left=$(kubectl get crd -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
            | grep -E "${pattern}" || true)
        [[ -z "${left}" ]] && break
        left_count=$(printf '%s\n' "${left}" | wc -l | tr -d ' ')
        if (( SECONDS - last_report >= 10 )); then
            info "Waiting for Crossplane/Upbound CRDs to terminate (${left_count} remaining)..."
            info "  Remaining: $(printf '%s' "${left}" | tr '\n' ' ' | sed 's/[[:space:]]\+$//')"
            last_report=${SECONDS}
        fi

        if (( aggressive_done == 0 && SECONDS - (deadline - 180) >= 45 )); then
            warn "Crossplane CRD deletion stalled; forcing cleanup of remaining CR instances/finalizers..."
            while IFS= read -r crd; do
                [[ -z "${crd}" ]] && continue

                # Clear any remaining custom resources that block CRD cleanup.
                _strip_and_delete_crd_instances "${crd}"

                kubectl patch crd "${crd}" \
                    --type=merge -p='{"metadata":{"finalizers":[]}}' \
                    2>/dev/null || true
                kubectl delete crd "${crd}" --ignore-not-found=true --wait=false 2>/dev/null || true
            done <<< "${left}"
            aggressive_done=1
        fi

        if (( SECONDS > deadline )); then
            warn "Some Crossplane/Upbound CRDs still remain after 180s."
            while IFS= read -r crd; do
                [[ -z "${crd}" ]] && continue
                kubectl patch crd "${crd}" \
                    --type=merge -p='{"metadata":{"finalizers":[]}}' \
                    2>/dev/null || true
            done <<< "${left}"
            break
        fi
        sleep 3
    done

    success "Crossplane/Upbound CRD sweep completed."

    # The CRDs above are Crossplane's own. A running Crossplane does not notice
    # they are gone and does not recreate them until it restarts, so a reinstall
    # that reuses this deployment finds its API missing. See
    # _purge_restart_crossplane for what that failure looks like.
    _purge_restart_crossplane
}

# Helper: strip finalizers via kubectl patch (standard CRD API path).
# Works reliably while the CRD is still registered.
_argocd_strip_kubectl() {
    local _crd="${1}"
    kubectl get "${_crd}" -n argocd -o name 2>/dev/null \
        | xargs_r -I{} kubectl patch {} -n argocd \
            --type=merge -p='{"metadata":{"finalizers":[]}}' \
            2>/dev/null || true
}

# Helper: strip finalizers via raw REST PUT.
# The Kubernetes API server deregisters CRD API endpoints asynchronously after
# CRD deletion, so this path remains reachable for a window after the CRD is
# gone.  It also catches objects missed by the kubectl path due to timing.
_argocd_strip_raw() {
    local _api_path="${1}"
    local _list _obj _name _clean
    _list=$(kubectl get --raw "${_api_path}" 2>/dev/null) || return 0
    printf '%s' "${_list}" \
        | jq -c '.items[] | select(.metadata.finalizers != null and (.metadata.finalizers | length) > 0)' \
        2>/dev/null \
        | while IFS= read -r _obj; do
            _name=$(printf '%s' "${_obj}" | jq -r '.metadata.name')
            _clean=$(printf '%s' "${_obj}" | jq '.metadata.finalizers = []')
            printf '%s\n' "${_clean}" \
                | kubectl replace --raw "${_api_path}/${_name}" -f - 2>/dev/null || true
        done
}

_has_pvc() {
    local ns="$1"
    [[ "$(kubectl get pvc -n "${ns}" --no-headers 2>/dev/null | wc -l)" -gt 0 ]]
}

# Strip all custom resource finalizers in a namespace so it can terminate.
# Uses kubectl get all + any remaining resources from a targeted list rather
# than iterating every api-resource (which hangs when CRDs are mid-deletion).
_clear_ns_finalizers() {
    local ns="$1"
    info "  Clearing finalizers in namespace ${ns}..."
    # Patch everything returned by 'kubectl get all' first (fast, covers core types).
    kubectl get all -n "${ns}" -o name 2>/dev/null \
        | xargs_r -I%% kubectl patch %% -n "${ns}" \
            --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
            2>/dev/null || true
    # Then try a fixed list of custom resource types we know may linger.
    for rt in \
        externalsecrets.external-secrets.io \
        clusterexternalsecrets.external-secrets.io \
        secretstores.external-secrets.io \
        terraforms.infra.contrib.fluxcd.io \
        releases.helm.crossplane.io \
        objects.kubernetes.crossplane.io \
        providerconfigs.kubernetes.crossplane.io \
        providerconfigs.helm.crossplane.io \
        providerconfigs.vault.upbound.io \
        managedresources.crossplane.io \
        certificates.cert-manager.io \
        issuers.cert-manager.io; do
        kubectl get "${rt}" -n "${ns}" -o name 2>/dev/null \
            | xargs_r -I%% kubectl patch %% -n "${ns}" \
                --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                2>/dev/null || true
    done
}

_delete_namespace() {
    local ns="$1"
    # IMPORTANT: --wait=false is required.  Without it, `kubectl delete namespace`
    # blocks indefinitely if the namespace has resources with stuck finalizers
    # (e.g. terraforms.infra.contrib.fluxcd.io after tf-controller is gone),
    # which prevents the polling loop below from ever running to strip them.
    kubectl delete namespace "${ns}" --ignore-not-found=true --grace-period=5 --wait=false 2>/dev/null || true
    # Wait up to 30s; if still Terminating, strip finalizers and retry.
    local deadline=$((SECONDS + 30))
    while kubectl get namespace "${ns}" --request-timeout=5s >/dev/null 2>&1; do
        if (( SECONDS > deadline )); then
            warn "  ${ns} stuck in Terminating — stripping remaining finalizers..."
            _clear_ns_finalizers "${ns}"
            # Also clear the namespace-level finalizer itself.
            kubectl get namespace "${ns}" -o json --request-timeout=10s 2>/dev/null \
              | jq '.spec.finalizers=[]' \
              | kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - 2>/dev/null || true
            break
        fi
        sleep 3
    done
    if kubectl get namespace "${ns}" --request-timeout=5s >/dev/null 2>&1; then
        warn "  ${ns} may still be terminating in the background."
    else
        success "  Deleted namespace: ${ns}"
    fi
}

# Delete all PVCs in a namespace and wait for the backing PVs to be Released/Deleted.
# Must be called BEFORE _delete_namespace so that:
#  (a) NFS directories are actually cleaned up (reclaimPolicy: Delete)
#  (b) PVC etcd entries are gone before the new install creates StatefulSets
#      with the same volumeClaimTemplate names — otherwise the new StatefulSets
#      bind to the surviving old PVCs and inherit stale data.
# Helm uninstall intentionally preserves StatefulSet volumeClaimTemplate PVCs.
_drain_pvcs() {
    local ns="$1"
    local pvcs
    pvcs=$(kubectl get pvc -n "${ns}" -o name 2>/dev/null) || return 0
    [[ -z "${pvcs}" ]] && return 0

    info "  Draining PVCs in ${ns} (terminating pods first)..."
    # Force-delete all pods so the pvc-protection controller can immediately
    # clear the kubernetes.io/pvc-protection finalizer.  Passive waiting (90s)
    # does not work for pods that were deployed by ArgoCD or other controllers
    # that are no longer running to orchestrate teardown.
    kubectl delete pods --all -n "${ns}" --grace-period=0 --force 2>/dev/null || true
    # Still wait up to 30s for pod objects to disappear (they do once kubelet
    # confirms the containers are gone) so the pvc-protection controller has
    # time to process the removal.
    kubectl wait pod --all -n "${ns}" --for=delete --timeout=30s 2>/dev/null || true

    info "  Deleting PVCs in ${ns}..."
    kubectl delete pvc --all -n "${ns}" --wait=true --timeout=120s 2>/dev/null || true

    # Belt-and-suspenders: strip any lingering pvc-protection finalizers so
    # that a timed-out delete doesn't block namespace termination.
    kubectl get pvc -n "${ns}" -o name 2>/dev/null \
        | xargs_r -I%% kubectl patch %% -n "${ns}" \
            --type=merge -p='{"metadata":{"finalizers":[]}}' \
            2>/dev/null || true
}

# Delete PVs that were previously bound to PVCs in a namespace.
# This is required for full data destruction when reclaimPolicy is Retain.
_delete_pvs_for_namespace() {
    local ns="$1"
    local pvs
    pvs=$(kubectl get pv -o json 2>/dev/null \
        | jq -r --arg ns "${ns}" '.items[] | select(.spec.claimRef.namespace == $ns) | .metadata.name') || return 0
    [[ -z "${pvs}" ]] && return 0

    info "  Deleting PVs previously bound to namespace ${ns}..."
    local pv deadline policy
    while IFS= read -r pv; do
        [[ -z "${pv}" ]] && continue

        # Reclaim policy first, and it is the whole point.
        #
        # Every kernel StorageClass reclaims with Retain, so deleting the PV
        # object removes Kubernetes' record of the volume and NOTHING else — the
        # disk stays allocated in the cloud project, invisible to the cluster
        # that created it. One orphan per volume per purge, until the provider
        # refuses to create any more:
        #
        #   CreateVolume failed: 413 VolumeLimitExceeded: Maximum number of
        #   volumes allowed (20) exceeded for quota 'volumes'
        #
        # and an install that has nothing to do with the volumes fails on a PVC
        # that will never bind. Switching to Delete before removing the object
        # is what makes the CSI driver call DeleteVolume.
        policy="$(kubectl get pv "${pv}" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}' 2>/dev/null || true)"
        if [[ "${policy}" != "Delete" ]]; then
            kubectl patch pv "${pv}" --type=merge \
                -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}' >/dev/null 2>&1 || true
        fi

        # Finalizers are NOT stripped up front. external-provisioner's finalizer
        # is what keeps the object alive until the driver has actually deleted
        # the disk; removing it first makes the PV disappear cleanly and leak the
        # volume just the same.
        kubectl delete pv "${pv}" --ignore-not-found=true --wait=false 2>/dev/null || true

        deadline=$(( SECONDS + 60 ))
        while kubectl get pv "${pv}" >/dev/null 2>&1; do
            if (( SECONDS >= deadline )); then
                warn "  PV ${pv} did not finalize in 60s — stripping finalizers."
                warn "    Its backing volume may survive in the cloud project; check the"
                warn "    provider console for a volume named ${pv}."
                kubectl patch pv "${pv}" \
                    --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                    2>/dev/null || true
                break
            fi
            sleep 2
        done
    done <<< "${pvs}"
}

# Every instance of a kind, as "<namespace>|<name>" lines. Cluster-scoped kinds
# yield an empty first field. One call covers both scopes: -A is ignored for a
# cluster-scoped kind rather than rejected.
#
# `|` rather than a space, and read with IFS='|'. Under the default IFS the
# leading blank of a cluster-scoped line is collapsed, so `read ns name` puts
# the name in ns and leaves name empty — a silent no-op on exactly the objects
# that block a CRD from being deleted.
#
# `-o name` cannot be used for this. With -A it prints kind/name and drops the
# namespace, so feeding it to kubectl silently addresses the `default`
# namespace — and --ignore-not-found then swallows every miss, so a delete that
# touched nothing reports exactly as one that worked.
_kind_instances() {
    local kind="$1"
    kubectl get "${kind}" -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"\n"}{end}' \
        2>/dev/null || true
}

# Strip finalizers and delete one object. An empty namespace means cluster-scoped.
_strip_and_delete_one() {
    local kind="$1" ns="$2" name="$3"
    [[ -n "${name}" ]] || return 0
    # Branching rather than an array of flags: bash 3.2 errors on an empty
    # array expansion under `set -u`, and macOS ships 3.2 (docs/plans §7).
    if [[ -n "${ns}" ]]; then
        kubectl patch "${kind}" "${name}" -n "${ns}" \
            --type=merge -p='{"metadata":{"finalizers":[]}}' 2>/dev/null || true
        kubectl delete "${kind}" "${name}" -n "${ns}" \
            --ignore-not-found=true --wait=false 2>/dev/null || true
    else
        kubectl patch "${kind}" "${name}" \
            --type=merge -p='{"metadata":{"finalizers":[]}}' 2>/dev/null || true
        kubectl delete "${kind}" "${name}" \
            --ignore-not-found=true --wait=false 2>/dev/null || true
    fi
}

# Strip finalizers and delete all instances of a CRD (cluster- and namespaced-scoped).
_strip_and_delete_crd_instances() {
    local crd="$1" ns name
    while IFS='|' read -r ns name; do
        _strip_and_delete_one "${crd}" "${ns}" "${name}"
    done < <(_kind_instances "${crd}")
}

# Delete CRDs whose names match an extended-regex pattern (e.g. 'gentianos\.io$').
_delete_crds_matching() {
    local pattern="$1"
    local label="${2:-CRDs matching ${pattern}}"
    local crds crd

    crds=$(kubectl get crd -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | grep -E "${pattern}" || true)
    if [[ -z "${crds}" ]]; then
        info "No ${label}; skipping."
        return 0
    fi

    info "Deleting ${label}..."
    while IFS= read -r crd; do
        [[ -z "${crd}" ]] && continue
        _strip_and_delete_crd_instances "${crd}"
        kubectl patch crd "${crd}" \
            --type=merge -p='{"metadata":{"finalizers":[]}}' \
            2>/dev/null || true
        kubectl delete crd "${crd}" --ignore-not-found=true --wait=false 2>/dev/null || true
    done <<< "${crds}"
    success "${label} removal queued."
}

_delete_envoy_gateway_scaffold() {
    local ns="${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"
    local gateway_class="${GENTIAN_GATEWAY_CLASS_NAME:-gentian-envoy}"

    info "Removing Gentian Gateway API edge scaffold..."

    # --all -A, not `get -o name | xargs delete`: -o name drops the namespace,
    # so every object outside `default` was left in place while the delete
    # reported success. That is what held gateway-exists-finalizer on the
    # GatewayClass, and the gatewayclass delete below then blocked forever.
    for rt in \
        backendtrafficpolicies.gateway.envoyproxy.io \
        clienttrafficpolicies.gateway.envoyproxy.io \
        securitypolicies.gateway.envoyproxy.io \
        envoyextensionpolicies.gateway.envoyproxy.io \
        envoypatchpolicies.gateway.envoyproxy.io \
        backendtlspolicies.gateway.networking.k8s.io \
        httproutes.gateway.networking.k8s.io \
        gateways.gateway.networking.k8s.io; do
        kubectl delete "${rt}" --all -A --ignore-not-found=true --wait=false 2>/dev/null || true
    done

    # The GatewayClass carries gateway-exists-finalizer, which Envoy Gateway
    # clears only once no Gateway references the class. So the Gateways have to
    # be gone first, and the controller has to still be running to notice —
    # which it is, until the helm uninstall below.
    local deadline=$(( SECONDS + 120 ))
    while kubectl get gateways.gateway.networking.k8s.io -A --no-headers 2>/dev/null | grep -q .; do
        if (( SECONDS >= deadline )); then
            warn "Gateways still present after 120s; stripping their finalizers."
            _strip_and_delete_crd_instances gateways.gateway.networking.k8s.io
            break
        fi
        sleep 3
    done

    kubectl delete gatewayclass "${gateway_class}" \
        --ignore-not-found=true --wait=false 2>/dev/null || true

    if helm status eg -n "${ns}" >/dev/null 2>&1; then
        info "Uninstalling Envoy Gateway Helm release (eg) in ${ns}..."
        helm uninstall eg -n "${ns}" --wait --timeout=3m 2>/dev/null || true
        success "Envoy Gateway Helm release uninstalled."
    else
        info "Envoy Gateway Helm release not found in ${ns}; skipping helm uninstall."
    fi

    _delete_namespace "${ns}"
    _delete_crds_matching 'gateway\.envoyproxy\.io$' 'Envoy Gateway extension CRDs'
    _delete_crds_matching 'gateway\.networking\.k8s\.io$' 'Gateway API CRDs'
}

# Called from A-09-argocd's destroy(), after Argo CD is gone.
#
# Not from C-04-mac-admission, which is where it lived: that step waits on
# Kyverno, it does not own it, and running there deleted Argo-owned objects
# while Argo was still reconciling them. A normal uninstall never needs this —
# C-02 removing gentian-appsets cascades through the Application finalizer and
# takes the chart with it. This is the fallback for a cluster whose Argo CD was
# already broken, and the only thing that removes the kyverno namespace and CRDs
# in that case.
_delete_kyverno_scaffold() {
    local policy

    info "Removing Gentian Kyverno admission scaffold..."
    for policy in \
        gentian-disallow-privileged \
        gentian-disallow-host-namespaces \
        gentian-require-non-root; do
        kubectl patch clusterpolicy "${policy}" \
            --type=merge -p='{"metadata":{"finalizers":[]}}' \
            2>/dev/null || true
        kubectl delete clusterpolicy "${policy}" --ignore-not-found=true --wait=false 2>/dev/null || true
    done
    success "Gentian Kyverno ClusterPolicies removed."

    # No helm uninstall. There has never been a release to uninstall: Kyverno is
    # an Argo CD Application, and Argo renders the chart and applies the
    # manifests rather than calling Helm. `helm status kyverno` missed every
    # time, and the branch logged "not found" as if that were a state worth
    # reporting.

    _delete_namespace "kyverno"
    _delete_crds_matching 'kyverno\.io$' 'Kyverno CRDs'
    # PolicyReport/ClusterPolicyReport live under wgpolicyk8s.io, not kyverno.io,
    # so the pattern above never matched them.
    _delete_crds_matching 'wgpolicyk8s\.io$' 'Policy report CRDs'

    # The part that matters most. Argo CD's cascade removes these webhook
    # configurations as chart resources on a healthy uninstall; when it did not
    # run, they outlive everything else here. They are cluster-scoped and fail
    # closed, so one left pointing at a service that no longer exists rejects
    # the creates the next install issues — and blocks the namespace deletes
    # A-03 is about to make.
    kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration -o name 2>/dev/null \
        | grep 'kyverno-' \
        | xargs_r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
    success "Kyverno webhook configurations removed."
}

_delete_gentianos_api_scaffold() {
    info "Removing Gentian OS API scaffold (CRs, CRDs, RBAC, webhooks)..."

    kubectl delete validatingwebhookconfiguration gentian-os-tenant-validator \
        --ignore-not-found=true 2>/dev/null || true

    if helm status gentian-os -n gentian-system >/dev/null 2>&1; then
        helm uninstall gentian-os -n gentian-system --wait --timeout=3m 2>/dev/null || true
        success "gentian-os Helm release uninstalled."
    fi

    _delete_crds_matching 'gentianos\.io$' 'gentianos.io CRDs'

    kubectl delete apiservice v1alpha1.gentianos.io --ignore-not-found=true 2>/dev/null || true

    kubectl delete clusterrolebinding \
        gentian-os \
        gentian-job-gc \
        --ignore-not-found=true 2>/dev/null || true
    kubectl get clusterrolebinding -o name 2>/dev/null \
        | grep -E 'gentian-portal' \
        | xargs_r kubectl delete --ignore-not-found=true 2>/dev/null || true

    kubectl delete clusterrole \
        gentian-os \
        gentian-job-gc \
        'crossplane:extra-resources:appprofiles.gentianos.io' \
        'crossplane:extra-resources:oidcpackcatalogs.gentianos.io' \
        'crossplane:extra-resources:externalsecrets.external-secrets.io' \
        --ignore-not-found=true 2>/dev/null || true
    kubectl get clusterrole -o name 2>/dev/null \
        | grep -E 'gentian-portal' \
        | xargs_r kubectl delete --ignore-not-found=true 2>/dev/null || true

    success "Gentian OS API scaffold removed."
}


# =============================================================================
# Purge — the data an uninstall deliberately keeps
#
# `--uninstall` reverses the steps: it removes what the installer created on the
# cluster and stops there. OpenBao's KV survives, which is the documented and
# usually correct behaviour — reinstalling onto the same cluster then recovers
# the credentials instead of re-prompting.
#
# `--purge` is for when that is exactly wrong: handing a cluster back, or
# proving a clean-room install. It removes the state the reverse pass keeps.
#
# Volumes outlive both. Every kernel StorageClass here reclaims with Retain, so
# deleting a PVC releases the PV and leaves the data on the backend — an
# "uninstalled" cluster whose OpenBao volume still holds every derived
# credential. PVs also outlive their namespace, since claimRef survives, which
# is why they are collected after the reverse pass rather than during it.
# =============================================================================

# purge_release_volumes — drain PVCs while their namespaces still exist.
#
# Runs before the reverse pass: a PVC whose pods are gone releases cleanly,
# and pvc-protection finalizers are the usual reason a namespace delete hangs.
# =============================================================================
# Crossplane's finalizers, cleared before anything that waits on them
# =============================================================================
#
# A purge used to deadlock here, permanently. The chain, observed on ifk-w4h:
#
#   Cluster claim ifk-w4h-prod   finalizer.apiextensions.crossplane.io
#     -> clusters.gentianos.io CRD        customresourcecleanup
#       -> XRDs xclusters / xsuze        offered + foregroundDeletion
#         -> Argo Applications crossplane-xrds, gentian-appsets
#           -> kubectl delete application, blocking with no timeout
#
# _delete_gentianos_api_scaffold already strips claim finalizers — but it runs
# from a step's destroy(), which is *after* the Application deletes that hang.
# So the helper existed and ran too late. This runs before the reverse pass.
#
# It cannot be left to Crossplane. Its composite controllers had tripped their
# circuit breaker ("Circuit breaker is open", controller=composite/...), so they
# had stopped reconciling those objects entirely and nothing would ever have
# removed the finalizers. Waiting was not slow, it was stopped.
#
# Order matters and is the reverse of ownership: managed resources, then claims,
# then composites, then the definitions. Taking a definition first strands its
# instances with no controller and no schema.
_purge_strip_crossplane_finalizers() {
    banner "Purge — clearing Crossplane finalizers"

    # 1. Managed resources. deletionPolicy: Orphan does NOT save these: Upjet
    #    calls Connect() before it decides to skip the external delete, so an
    #    Orphan resource whose backend is already torn down retries forever.
    #    Four Keycloak MRs hung exactly this way, with Keycloak gone and
    #    provider-keycloak reporting "cannot get terraform setup".
    local mr count
    count=0
    while IFS='|' read -r _ mr; do
        [[ -n "${mr}" ]] || continue
        count=$((count + 1))
    done < <(kubectl get managed -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
    if [[ "${count}" -gt 0 ]]; then
        info "Clearing finalizers on ${count} Crossplane managed resource(s)..."
        kubectl get managed -o name 2>/dev/null | while IFS= read -r obj; do
            [[ -n "${obj}" ]] || continue
            kubectl patch "${obj}" --type=merge \
                -p='{"metadata":{"finalizers":[]}}' 2>/dev/null || true
        done
        success "Managed resource finalizers cleared."
    else
        info "No Crossplane managed resources; skipping."
    fi

    # 2. Claims, then 3. composites. Both are gentianos.io kinds served by the
    #    XRDs below, so they must go before the definitions that describe them.
    local kind
    for kind in $(kubectl api-resources --api-group=gentianos.io -o name 2>/dev/null || true); do
        _strip_and_delete_crd_instances "${kind}"
    done

    # 4. The definitions. foregroundDeletion holds an XRD until every dependent
    #    is gone, which is why the three steps above come first.
    local xrd
    for xrd in $(kubectl get compositeresourcedefinitions -o name 2>/dev/null || true); do
        kubectl patch "${xrd}" --type=merge \
            -p='{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    done
    success "Crossplane finalizers cleared."
}

# _purge_restart_crossplane — give Crossplane back the CRDs the purge removed.
#
# Crossplane v2 ships no CRDs in its Helm chart: `helm template` renders zero,
# and the core binary installs them itself at startup. So a purge that deletes
# them leaves a running Crossplane whose own API is missing, and a reinstall
# that finds `helm status crossplane` satisfied never restores them.
#
# What that looks like, if this does not run: every provider and function sits
# Installed=True Healthy=False, each revision reporting
#
#   cannot establish control of object: the server could not find the requested
#   resource (post managedresourcedefinitions.apiextensions.crossplane.io)
#
# and the install blocks in `kubectl wait --for=condition=Healthy` until it
# times out. A restart fixes it in seconds, because startup is when those CRDs
# are created. The default DeploymentRuntimeConfig comes back with them.
_purge_restart_crossplane() {
    kubectl -n crossplane-system get deploy crossplane >/dev/null 2>&1 || return 0
    info "Restarting Crossplane so it reinstalls the CRDs this purge removed..."
    kubectl -n crossplane-system rollout restart deploy/crossplane >/dev/null 2>&1 || true
    kubectl -n crossplane-system rollout status deploy/crossplane --timeout=180s >/dev/null 2>&1 || true
    success "Crossplane restarted."
}

# _purge_guard_single_instance — refuse a second concurrent purge.
#
# Two purges ran against this cluster at once and nothing objected; both then
# blocked on the same `kubectl delete application`, each making the other's
# wait longer. Nothing about the teardown is safe to interleave with itself.
_purge_guard_single_instance() {
    local lock="${TMPDIR:-/tmp}/gentian-purge-${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-cluster}.lock"
    local holder
    if [[ -e "${lock}" ]]; then
        holder=$(cat "${lock}" 2>/dev/null || echo "")
        if [[ -n "${holder}" ]] && kill -0 "${holder}" 2>/dev/null; then
            error "A purge is already running for this cluster (pid ${holder})."
            error "  Two teardowns interleaved is not a state this can reason about."
            error "  Wait for it, or remove ${lock} if that process is gone."
            return 1
        fi
        info "Stale purge lock from pid ${holder:-unknown}; taking it over."
    fi
    echo "$$" > "${lock}" 2>/dev/null || true
    # shellcheck disable=SC2064
    trap "rm -f '${lock}'" EXIT
    return 0
}

purge_release_volumes() {
    local ns
    # Before the volumes, because the reverse pass that follows deletes the Argo
    # Applications that these finalizers block.
    _purge_strip_crossplane_finalizers
    banner "Purge — releasing volumes"
    for ns in $(gentian_kernel_namespaces); do
        kubectl get namespace "${ns}" >/dev/null 2>&1 || continue
        _has_pvc "${ns}" || continue
        _drain_pvcs "${ns}"
    done
}

# purge_delete_volumes — delete the PVs the drain released.
#
# After the reverse pass, because a Released PV keeps its claimRef and so can
# still be attributed to the namespace it served.
purge_delete_volumes() {
    local ns
    banner "Purge — deleting released volumes"
    for ns in $(gentian_kernel_namespaces); do
        _delete_pvs_for_namespace "${ns}"
    done
}

# purge_local_state — the installer's own files.
#
# Not install.env: that is the operator's input, surface 1, and the one thing a
# reinstall legitimately starts from. Everything here is derived — a plugin
# config this run wrote, a cache with no cluster left to seed, and init files
# holding unseal keys for storage that no longer exists.
purge_local_state() {
    banner "Purge — local state"
    local f
    for f in "${HOME}/.gentian/config" \
             "${GENTIAN_CREDENTIAL_CACHE:-${HOME}/.gentian/bootstrap-credentials.env}" \
             "${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}" \
             "${TRANSIT_INIT_FILE:-${HOME}/.gentian/openbao-transit-init.json}" \
             "/tmp/openbao-init.json" \
             "/tmp/openbao-transit-init.json"; do
        [[ -e "${f}" ]] || continue
        rm -f "${f}" && success "Removed ${f}."
    done
}

# purge_report_remaining — what a purge does not touch, said out loud.
#
# The cluster's configuration is in Git and is not this command's to delete:
# it is shared with every other cluster in the repository, and removing it is a
# commit somebody reviews. Saying so is the difference between "purged" and
# "purged, except the part that would rebuild it identically".
# =============================================================================
# Cluster infrastructure — the shared operators Gentian brings up alongside it
# =============================================================================
#
# CNPG and Reloader arrive as Argo CD Applications (B-03) and survive both an
# uninstall and a plain purge, for two reasons that have nothing to do with Argo
# being alive to prune:
#
#   Their CRDs carry helm.sh/resource-policy: keep, so Helm deliberately leaves
#   them behind — it protects data by refusing to remove the definitions of the
#   objects holding it.
#
#   Their namespaces come from syncOptions: CreateNamespace=true, which creates
#   them OUTSIDE the Application's resource tree. Deleting the Application never
#   prunes them, and nothing else names them.
#
# So a pruning Argo removes the workloads and leaves the namespace and the CRDs
# standing.
#
# Opt-in, via --purge --cluster-infra, because "this installer created it" and
# "this cluster can lose it" are different questions. CNPG's CRDs define every
# Postgres on the machine; a cluster that ran CNPG before Gentian keeps serving
# other workloads from it, and removing the definitions takes those with it.
# That is a judgement about the cluster, which the person purging it holds and
# this script does not.
#
# Never on --uninstall, at any flag. An uninstall keeps data by design, and
# these CRDs define the objects the data lives in.

# Namespaces the bootstrap Applications deploy into, read from the Applications
# themselves rather than from a list kept alongside them. A list would drift the
# first time an add-on is added, and drift silently, because nothing fails when
# a teardown forgets a namespace.
_cluster_infra_namespaces() {
    local tmpl ns

    # Checked before the loop, not inside it: `for ... done | sort -u` runs the
    # loop in a subshell, so a flag set in there never reaches this function.
    #
    # A glob that matches nothing is indistinguishable from a set of
    # Applications that deploy nowhere — both produce no output and no error.
    # The chart moved once already, and --cluster-infra silently removed
    # nothing until someone noticed.
    local -a templates=()
    for tmpl in "${SCRIPT_DIR}"/kernel/bootstrap/chart/templates/*.yaml; do
        [[ -f "${tmpl}" ]] && templates+=("${tmpl}")
    done
    if [[ ${#templates[@]} -eq 0 ]]; then
        warn "No bootstrap Application templates under kernel/bootstrap/chart/templates —" \
             "cluster infrastructure namespaces cannot be determined and will be left in place."
        return 0
    fi

    for tmpl in "${templates[@]}"; do
        # The namespace on the line after `destination:`, which is where an
        # Argo CD Application declares where it deploys. Read from the chart
        # template rather than a rendered manifest: destination.namespace is a
        # literal in every one of them, and rendering would need the values a
        # teardown no longer has.
        ns="$(awk '/^  destination:/{f=1; next} f && /namespace:/{print $2; exit}' "${tmpl}")"
        [[ -n "${ns}" ]] || continue
        # Namespaces owned by a step are that step's to remove, and A-03 already
        # does. Only the ones nothing else claims belong here.
        case "${ns}" in
            argocd|openbao|platform-kernel|gentian-system|gentian-*) continue ;;
        esac
        echo "${ns}"
    done | sort -u
}

# CRD groups those operators register. Declared, because a CRD carries no record
# of which release installed it — helm.sh/resource-policy: keep is the only mark
# on them, and cert-manager and Crossplane wear it too.
#
# Anything missing here is reported by the sweep below rather than left silent.
_cluster_infra_crd_patterns() {
    printf '%s\n' \
        'cnpg\.io$' \
        'postgresql\.cnpg\.io$'
}

purge_cluster_infra() {
    local ns pattern
    banner "Cluster infrastructure"

    if [[ "${DRY_RUN:-0}" == "1" || "${GENTIAN_DRY_RUN:-0}" == "1" ]]; then
        info "  [dry-run] Would remove: $(_cluster_infra_namespaces | tr '\n' ' ')"
        return 0
    fi

    for ns in $(_cluster_infra_namespaces); do
        _delete_namespace "${ns}"
    done

    for pattern in $(_cluster_infra_crd_patterns); do
        _delete_crds_matching "${pattern}" "cluster infrastructure CRDs (${pattern})"
    done

    # Webhook configurations outlive both, and a stale one whose service is gone
    # rejects every create for the resources it intercepts.
    kubectl get validatingwebhookconfiguration,mutatingwebhookconfiguration -o name 2>/dev/null \
        | grep -E 'cnpg|reloader|stakater' \
        | xargs_r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true

    success "Cluster infrastructure removed."
}

# What a purge leaves, stated rather than discovered later. Silence here is the
# failure mode this whole teardown path kept producing.
purge_report_cluster_residue() {
    local left
    # `|| true` is load-bearing. grep exits 1 when it filters everything out,
    # and under `set -o pipefail` that failed the whole substitution — so the
    # function whose job is to report leftovers aborted the run precisely when
    # there were none, after a purge that had already succeeded.
    left="$(kubectl get crd -o name 2>/dev/null \
        | sed 's#.*/##' \
        | grep -vE '\.k8s\.io$|\.kubernetes\.io$|cilium\.io$' \
        | sed 's/^[^.]*\.//' | sort -u | tr '\n' ' ' || true)"
    [[ -n "${left// /}" ]] || return 0
    warn "CRD groups still registered: ${left}"
    warn "  Remove them with --cluster-infra, or leave them if another workload uses them."
}

purge_report_remaining() {
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-<cluster>}"
    echo ""
    info "Left in place, deliberately:"
    info "  install.env — your input, and what a reinstall starts from."
    info "  clusters/${cluster}/kernel in gentian-deployments — this cluster's"
    info "    configuration. Remove it with a commit if the cluster is gone for good."
    info "  Anything another workload put on this cluster."
}

# purge_confirm — the one prompt in the teardown path.
#
# Everything else the installer destroys is re-derivable: manifests come from
# Git, and every derived credential is a function of the master password and
# the derivation salt. The salt is generated at first install and stored only in
# OpenBao, so deleting OpenBao's volume ends the derivation — the same master
# password then produces different credentials, and a rebuild is a migration
# rather than a restore. `--export-recovery-kit` is what closes that gap, so
# whether one exists decides how bad this is.
purge_confirm() {
    _purge_guard_single_instance || return 1
    echo ""
    warn "PURGE removes what an uninstall keeps:"
    warn "  • OpenBao's storage — every credential, and the derivation salt"
    warn "  • the infra database, cache and object-store volumes"
    warn "  • ~/.gentian and the OpenBao init files on this machine"
    if [[ "${GENTIAN_PURGE_CLUSTER_INFRA:-0}" == "1" ]]; then
        warn "  • --cluster-infra: the shared operators this installer brought up —"
        warn "    CNPG, Reloader — and their CRDs. Every Postgres cluster on this"
        warn "    machine goes with them, not only Gentian's."
    fi
    echo ""
    warn "Without a recovery kit the salt is gone with it, and the same master"
    warn "password will not reproduce this cluster's credentials. Export one first:"
    warn "  ./install.sh --export-recovery-kit"
    echo ""

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        # A prompt nobody can answer would hang a CI job forever, and defaulting
        # to yes on a destructive path is not a default.
        error "--purge needs confirmation and GENTIAN_NONINTERACTIVE=1 cannot give it."
        error "  Set GENTIAN_PURGE_CONFIRM=${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-<cluster-id>} to run unattended."
        return 1
    fi

    local expected="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}"
    local answer
    read -rp "  Type the cluster id (${expected}) to confirm: " answer
    if [[ "${answer}" != "${expected}" || -z "${expected}" ]]; then
        error "Cluster id did not match — nothing was purged."
        return 1
    fi
    return 0
}
