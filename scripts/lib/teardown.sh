#!/usr/bin/env bash
# =============================================================================
# scripts/lib/teardown.sh — teardown primitives
# =============================================================================
# Shared teardown helpers called by step destroy(). They lived in uninstall.sh, which no longer
# exists — uninstall is the driver run in reverse, so the helpers have to be library code.
# =============================================================================

[[ -n "${GENTIAN_TEARDOWN_LOADED:-}" ]] && return 0
GENTIAN_TEARDOWN_LOADED=1

_mr_count() {
    # Under strict mode, `kubectl get managed` can return non-zero if the
    # API alias is unavailable; treat that as "no managed resources".
    local mr_list
    if mr_list="$(kubectl get managed --no-headers 2>/dev/null)"; then
        if [[ -z "${mr_list}" ]]; then
            echo 0
        else
            printf '%s\n' "${mr_list}" | wc -l | tr -d ' '
        fi
    else
        echo 0
    fi
}

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
                while IFS= read -r obj; do
                    [[ -z "${obj}" ]] && continue
                    # Ignore unexpected lines; valid resources are kind/name.
                    [[ "${obj}" != */* ]] && continue
                    kubectl patch "${obj}" \
                        --type=merge -p='{"metadata":{"finalizers":[]}}' \
                        2>/dev/null || true
                    kubectl delete "${obj}" --ignore-not-found=true --wait=false 2>/dev/null || true
                done < <(
                    kubectl get "${crd}" -A -o name 2>/dev/null || true
                    kubectl get "${crd}" -o name 2>/dev/null || true
                )

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
}

# Helper: strip finalizers via kubectl patch (standard CRD API path).
# Works reliably while the CRD is still registered.
_argocd_strip_kubectl() {
    local _crd="${1}"
    kubectl get "${_crd}" -n argocd -o name 2>/dev/null \
        | xargs -r -I{} kubectl patch {} -n argocd \
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
        | xargs -r -I%% kubectl patch %% -n "${ns}" \
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
            | xargs -r -I%% kubectl patch %% -n "${ns}" \
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
        | xargs -r -I%% kubectl patch %% -n "${ns}" \
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
    while IFS= read -r pv; do
        [[ -z "${pv}" ]] && continue
        kubectl patch pv "${pv}" \
            --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
            2>/dev/null || true
        kubectl delete pv "${pv}" --ignore-not-found=true --wait=false 2>/dev/null || true
    done <<< "${pvs}"
}

# Strip finalizers and delete all instances of a CRD (cluster- and namespaced-scoped).
_strip_and_delete_crd_instances() {
    local crd="$1"
    while IFS= read -r obj; do
        [[ -z "${obj}" ]] && continue
        [[ "${obj}" != */* ]] && continue
        kubectl patch "${obj}" \
            --type=merge -p='{"metadata":{"finalizers":[]}}' \
            2>/dev/null || true
        kubectl delete "${obj}" --ignore-not-found=true --wait=false 2>/dev/null || true
    done < <(
        kubectl get "${crd}" -A -o name 2>/dev/null || true
        kubectl get "${crd}" -o name 2>/dev/null || true
    )
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

    for rt in \
        backendtrafficpolicies.gateway.envoyproxy.io \
        clienttrafficpolicies.gateway.envoyproxy.io \
        securitypolicies.gateway.envoyproxy.io \
        envoyextensionpolicies.gateway.envoyproxy.io \
        envoypatchpolicies.gateway.envoyproxy.io \
        backendtlspolicies.gateway.networking.k8s.io; do
        kubectl get "${rt}" -A -o name 2>/dev/null \
            | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
    done

    kubectl get httproute -A -o name 2>/dev/null \
        | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
    kubectl get gateway -A -o name 2>/dev/null \
        | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
    kubectl delete gatewayclass "${gateway_class}" --ignore-not-found=true 2>/dev/null || true

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

    if helm status kyverno -n kyverno >/dev/null 2>&1; then
        info "Uninstalling Kyverno Helm release..."
        helm uninstall kyverno -n kyverno --wait --timeout=3m 2>/dev/null || true
        success "Kyverno Helm release uninstalled."
    else
        info "Kyverno Helm release not found; skipping helm uninstall."
    fi

    _delete_namespace "kyverno"
    _delete_crds_matching 'kyverno\.io$' 'Kyverno CRDs'

    # Belt-and-suspenders: helm uninstall normally removes Kyverno's webhook
    # configs as chart-managed resources, but not if the Helm release was
    # already broken/missing above (helm uninstall never ran). These are
    # cluster-scoped and fail closed, so leaving them behind blocks the next
    # install — explicitly sweep them regardless of how the helm step went.
    kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration -o name 2>/dev/null \
        | grep 'kyverno-' \
        | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
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
        | xargs -r kubectl delete --ignore-not-found=true 2>/dev/null || true

    kubectl delete clusterrole \
        gentian-os \
        gentian-job-gc \
        'crossplane:extra-resources:appprofiles.gentianos.io' \
        --ignore-not-found=true 2>/dev/null || true
    kubectl get clusterrole -o name 2>/dev/null \
        | grep -E 'gentian-portal' \
        | xargs -r kubectl delete --ignore-not-found=true 2>/dev/null || true

    success "Gentian OS API scaffold removed."
}

_remove_host_cli() {
    local path="$1"
    if [[ ! -e "${path}" && ! -L "${path}" ]]; then
        return 0
    fi
    if [[ -w /usr/local/bin ]]; then
        rm -f "${path}"
    elif sudo rm -f "${path}"; then
        :
    else
        warn "Failed to remove ${path} — remove manually."
        return 1
    fi
    success "Removed ${path}."
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
purge_release_volumes() {
    local ns
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
             "${OPENBAO_INIT_FILE:-/tmp/openbao-init.json}" \
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
    echo ""
    warn "PURGE removes what an uninstall keeps:"
    warn "  • OpenBao's storage — every credential, and the derivation salt"
    warn "  • the infra database, cache and object-store volumes"
    warn "  • ~/.gentian and the OpenBao init files on this machine"
    echo ""
    warn "Without a recovery kit the salt is gone with it, and the same master"
    warn "password will not reproduce this cluster's credentials. Export one first:"
    warn "  ./install.sh --export-recovery-kit kit.age"
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
