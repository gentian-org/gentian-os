#!/usr/bin/env bash
# =============================================================================
# uninstall.sh — Gentian OS Crossplane-based uninstall
# =============================================================================
# Reverses install.sh (Phase 1 + Phase 2 bootstrap) in reverse order.
#
# Default (safe) mode:
#   - Removes the Nubus provider-helm Release and associated Secrets/ConfigMaps
#   - Removes the Cluster XR (waits for Crossplane GC)
#   - Removes Crossplane resources (XRD, Composition, providers)
#   - Uninstalls Crossplane core
#   - Removes ArgoCD, ESO, cert-manager
#   - Preserves PVC/PV data and namespaces that contain PVCs
#   - Preserves OpenBao KV paths (managementPolicies: Observe, Create)
#
# Force mode (-f):
#   - All safe-mode steps
#   - Also deletes data namespaces and bound PVs (full teardown)
#
# Usage:
#   ./uninstall-cp.sh            # safe teardown
#   ./uninstall-cp.sh -f         # full teardown (DESTROYS DATA)
#   ./uninstall-cp.sh --cluster-infra  # also remove cert-manager/CNPG/reloader
# =============================================================================

set -euo pipefail

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
banner()  { echo -e "\n${CYAN}══════════════════════════════════════════════════${NC}"; echo -e "${CYAN}  $*${NC}"; echo -e "${CYAN}══════════════════════════════════════════════════${NC}\n"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODE="safe"
UNINSTALL_CLUSTER_INFRA=0
INSTALL_STATE_FILE="${INSTALL_STATE_FILE:-${SCRIPT_DIR}/.install-state.env}"
GENTIAN_MANAGED_CERT_MANAGER="${GENTIAN_MANAGED_CERT_MANAGER:-0}"

# Load install state to know if cert-manager is Gentian-managed.
# shellcheck source=/dev/null
[[ -r "${INSTALL_STATE_FILE}" ]] && source "${INSTALL_STATE_FILE}" || true

while [[ $# -gt 0 ]]; do
    case "$1" in
        -f)                MODE="force" ;;
        --cluster-infra)   UNINSTALL_CLUSTER_INFRA=1 ;;
        --no-cluster-infra) UNINSTALL_CLUSTER_INFRA=0 ;;
        -h|--help)
            echo "Usage: $0 [-f] [--cluster-infra]"
            echo "  default        : safe uninstall (preserve PVC/PV data)"
            echo "  -f             : force uninstall (delete namespaces + bound PVs)"
            echo "  --cluster-infra: also remove cert-manager/reloader/CNPG"
            exit 0
            ;;
        *)
            error "Unknown option: $1"
            exit 1
            ;;
    esac
    shift
done

for cmd in kubectl helm; do
    command -v "$cmd" >/dev/null 2>&1 || { error "Missing: $cmd"; exit 1; }
done

echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║     Gentian OS — Crossplane Uninstall                    ║${NC}"
if [[ "${MODE}" == "force" ]]; then
    echo -e "${RED}║     MODE: FORCE (all data will be deleted!)              ║${NC}"
else
    echo -e "${CYAN}║     MODE: SAFE  (PVC/PV data preserved)                  ║${NC}"
fi
echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""

if [[ "${MODE}" == "force" ]]; then
    warn "FORCE mode: this will permanently delete all Gentian OS data."
    read -rp "  Type 'yes' to confirm: " confirm
    [[ "${confirm}" == "yes" ]] || { info "Aborted."; exit 0; }
fi

# =============================================================================
# Step 1 — Remove Cluster XR claim and wait for Crossplane GC
# managementPolicies: [Observe, Create] on KV seeds means Crossplane will NOT
# delete the OpenBao KV paths when the XR is deleted. All other MRs
# (namespaces, policies, auth backend, etc.) ARE deleted by Crossplane GC.
# =============================================================================
banner "Step 1 — Remove Cluster XR (Crossplane GC)"

if kubectl get cluster dev-cluster -n crossplane-system >/dev/null 2>&1; then
    info "Deleting Cluster claim dev-cluster..."
    kubectl delete cluster dev-cluster -n crossplane-system --timeout=60s || true
else
    info "Cluster claim dev-cluster not found; skipping."
fi

if kubectl get xcluster dev-cluster >/dev/null 2>&1; then
    info "Waiting for XCluster dev-cluster to be garbage-collected (max 5m)..."
    local_deadline=$((SECONDS + 300))
    while kubectl get xcluster dev-cluster >/dev/null 2>&1; do
        if (( SECONDS > local_deadline )); then
            warn "XCluster dev-cluster still present after 5m — forcing deletion."
            kubectl delete xcluster dev-cluster --grace-period=0 --force >/dev/null 2>&1 || true
            break
        fi
        sleep 5
    done
    success "XCluster dev-cluster removed."
else
    info "XCluster dev-cluster not found; skipping."
fi

# =============================================================================
# Step 1b — Remove Phase 2 resources (provider-helm Release, nubus Secrets/CMs)
# Must run before Crossplane providers are removed so the Release GC can run.
# =============================================================================
banner "Step 1b — Remove Nubus provider-helm Release"

if kubectl get release.helm.crossplane.io/nubus-dev >/dev/null 2>&1; then
    info "Deleting provider-helm Release nubus-dev..."
    kubectl delete release.helm.crossplane.io/nubus-dev --timeout=60s || true
    info "Waiting for Helm GC (nubus-dev uninstall, max 3m)..."
    local_deadline=$((SECONDS + 180))
    while kubectl get release.helm.crossplane.io/nubus-dev >/dev/null 2>&1; do
        if (( SECONDS > local_deadline )); then
            warn "Release nubus-dev still present after 3m — forcing finalizer removal."
            kubectl patch release.helm.crossplane.io/nubus-dev \
                --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                2>/dev/null || true
            break
        fi
        sleep 5
    done
    success "Release nubus-dev removed."
else
    info "Release nubus-dev not found; skipping."
fi

for ns in gentian-dev gentian-infra-dev; do
    info "Removing nubus ConfigMaps / Secrets from ${ns}..."
    kubectl delete configmap \
        nubus-base-values \
        nubus-dev-values \
        nubus-dev-udm-listener-nats-patch \
        -n "${ns}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete externalsecret \
        nubus-credentials \
        nubus-sensitive-values \
        -n "${ns}" --ignore-not-found=true 2>/dev/null || true
    # ESO-owned Secrets: delete only if ExternalSecrets are gone
    kubectl delete secret \
        nubus-credentials \
        nubus-sensitive-values \
        -n "${ns}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret registry-credentials \
        -n "${ns}" --ignore-not-found=true 2>/dev/null || true
done
success "Phase 2 nubus resources removed."

# =============================================================================
# Step 1c — Drain all remaining Crossplane managed resources
#
# After the XR is GC'd the vault/kubernetes provider controllers start
# finalizing the composed MRs. With managementPolicies:[Observe,Create] they
# skip the external delete and just strip the finalizer — but this can be slow
# or stuck if the provider pod is busy or OpenBao is unreachable.
#
# We wait up to 60 s for MRs to drain on their own, then force-strip any
# remaining finalizers so the subsequent ProviderConfig deletes don't hang.
# =============================================================================
banner "Step 1c — Drain Crossplane managed resources"

_mr_count() {
    kubectl get managed --no-headers 2>/dev/null | wc -l | tr -d ' '
}

info "Waiting up to 60s for managed resources to drain..."
drain_deadline=$((SECONDS + 60))
while [[ "$(_mr_count)" -gt 0 ]]; do
    if (( SECONDS > drain_deadline )); then
        warn "Managed resources still present after 60s — stripping finalizers."
        break
    fi
    sleep 5
done

remaining_count="$(_mr_count)"
if [[ "${remaining_count}" -gt 0 ]]; then
    warn "  ${remaining_count} managed resource(s) still present — forcing finalizer removal."
    while IFS= read -r mr; do
        [[ -z "${mr}" ]] && continue
        kubectl patch "${mr}" \
            --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
            2>/dev/null || true
        kubectl delete "${mr}" --grace-period=0 --force \
            --ignore-not-found=true 2>/dev/null || true
    done < <(kubectl get managed -o name 2>/dev/null)
    success "  Finalizers stripped; managed resources removed."
else
    success "  All managed resources drained."
fi

# =============================================================================
# Step 2 — Remove Crossplane compositions, XRDs, and ProviderConfigs
# =============================================================================
banner "Step 2 — Remove Crossplane XRDs, Compositions, ProviderConfigs"

for file in \
    "${SCRIPT_DIR}/crossplane/compositions/cluster-default.yaml" \
    "${SCRIPT_DIR}/crossplane/xrds/cluster.yaml"
do
    if [[ -f "${file}" ]]; then
        kubectl delete -f "${file}" --ignore-not-found=true
        success "  Removed: $(basename "${file}")"
    fi
done

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

_delete_provider_config "providerconfig.kubernetes.crossplane.io/kubernetes" "provider-kubernetes/kubernetes"
_delete_provider_config "providerconfig.helm.crossplane.io/kubernetes"       "provider-helm/kubernetes"
_delete_provider_config "providerconfig.vault.upbound.io/openbao"            "provider-vault/openbao"

# =============================================================================
# Step 3 — Uninstall Crossplane providers
# =============================================================================
banner "Step 3 — Uninstall Crossplane providers"

for provider in \
    function-go-templating \
    function-auto-ready \
    provider-helm \
    provider-kubernetes \
    provider-vault
do
    kubectl delete "function.pkg.crossplane.io/${provider}" \
        --ignore-not-found=true 2>/dev/null \
    || kubectl delete "provider.pkg.crossplane.io/${provider}" \
        --ignore-not-found=true 2>/dev/null \
    || true
    success "  ${provider} removed."
done

# =============================================================================
# Step 4 — Uninstall Crossplane core
# =============================================================================
banner "Step 4 — Uninstall Crossplane core"

if helm status crossplane -n crossplane-system >/dev/null 2>&1; then
    helm uninstall crossplane -n crossplane-system
    success "Crossplane Helm release uninstalled."
elif kubectl get deployment crossplane -n crossplane-system >/dev/null 2>&1; then
    warn "Crossplane deployment present but not Helm-managed (e.g. microk8s addon)."
    info "Skipping Helm uninstall — disable the addon manually if needed:"
    info "  microk8s disable crossplane"
else
    info "Crossplane not found; skipping."
fi

# Clean up cluster-scoped RBAC (left behind by either Helm or addon installs).
kubectl delete clusterrole \
    crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
    --ignore-not-found=true 2>/dev/null || true
kubectl delete clusterrolebinding \
    crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
    --ignore-not-found=true 2>/dev/null || true
kubectl delete namespace crossplane-system --ignore-not-found=true 2>/dev/null || true
success "Crossplane cluster-scoped resources cleaned up."

# =============================================================================
# Step 5 — Remove derived-credential Secrets from crossplane-system
# (these are not cluster data — safe to always remove)
# =============================================================================
banner "Step 5 — Remove Crossplane input Secrets"

for secret in \
    gentian-os-master-password \
    gentian-os-kernel-database-postgresql \
    gentian-os-kernel-database-mariadb \
    gentian-os-kernel-cache-redis \
    gentian-os-kernel-storage-minio \
    gentian-os-kernel-identity-nubus \
    gentian-os-kernel-identity-keycloak-bootstrap \
    gentian-os-kernel-mail-postfix \
    registry-credentials-helm
do
    kubectl delete secret "${secret}" -n crossplane-system \
        --ignore-not-found=true 2>/dev/null || true
done
success "Derived-credential and registry Secrets removed."

# =============================================================================
# Step 6 — Uninstall ArgoCD
#
# Root cause of the recurring stuck argocd namespace:
#   Application objects carry resources-finalizer.argocd.argoproj.io.
#   After Helm uninstall the ArgoCD app-controller is gone so no process
#   ever removes the finalizer; namespace GC stalls indefinitely.
#
# Correct teardown sequence (two independent finalizer-strip paths):
#  1. helm uninstall — kills all ArgoCD controllers, prevents finalizer re-add
#  2. Brief pause so reconcile goroutines exit
#  3a. kubectl patch path — standard CRD API; reliable while CRD is alive
#  3b. Raw REST PUT path — works even after CRD deletion because the API
#      server deregisters CRD endpoints asynchronously (--wait=false returns
#      before the endpoint is torn down, and etcd objects remain accessible)
#  4. Delete CRDs (--wait=false, non-blocking)
#  5. Second raw REST sweep to catch any orphans that slipped through step 3
#  6. kubectl delete namespace argocd
#  7. Immediately force-clear spec.finalizers via /finalize subresource
#  8. Poll ≤30 s; force-finalize again if namespace still lingers
# =============================================================================
banner "Step 6 — Uninstall ArgoCD"

# 1. Kill all ArgoCD controllers first — live controller reconciles within
#    seconds and re-adds resources-finalizer.argocd.argoproj.io to every app.
if helm status argocd -n argocd >/dev/null 2>&1; then
    info "Uninstalling ArgoCD Helm release..."
    helm uninstall argocd -n argocd
    success "  ArgoCD Helm release uninstalled."
fi

# 2. Give controller goroutines a moment to exit before we strip finalizers.
sleep 3

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

# 3a. kubectl path (needs CRD alive — run before CRD deletion).
if kubectl get crd applications.argoproj.io >/dev/null 2>&1; then
    info "Stripping ArgoCD CR finalizers (kubectl path)..."
    _argocd_strip_kubectl applications.argoproj.io
    _argocd_strip_kubectl applicationsets.argoproj.io
    _argocd_strip_kubectl appprojects.argoproj.io
    success "  kubectl path done."
fi

# 3b. Raw REST path (belt-and-suspenders; also works after CRD deletion).
info "Stripping ArgoCD CR finalizers (raw API path)..."
_argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications"
_argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/applicationsets"
_argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects"
success "  Raw API path done."

# 4. Delete CRDs without blocking.  Objects with finalizers already cleared
#    above are GC'd immediately; orphans are caught in the step-5 sweep.
info "Deleting ArgoCD CRDs..."
kubectl get crd -o name 2>/dev/null | grep argoproj.io \
    | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
success "  ArgoCD CRDs queued for deletion."

# 5. Second raw-REST sweep — catches objects that still carry finalizers while
#    the API server is still serving the endpoint in the teardown window.
sleep 2
_argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications"
_argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/applicationsets"
_argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects"

# 6. Delete the namespace.
kubectl delete namespace argocd --ignore-not-found=true 2>/dev/null || true

# 7. Immediately force-clear spec.finalizers via the /finalize subresource.
#    This bypasses the content-check that blocks normal namespace deletion,
#    allowing Kubernetes to remove the namespace from etcd even when orphan
#    objects with unknown finalizers are still present.
info "Force-finalizing argocd namespace..."
kubectl get namespace argocd -o json --request-timeout=10s 2>/dev/null \
    | jq '.spec.finalizers = []' \
    | kubectl replace --raw "/api/v1/namespaces/argocd/finalize" -f - \
    2>/dev/null || true

# 8. Poll until gone; force-finalize again if namespace still lingers.
_t=$((SECONDS + 30))
while kubectl get namespace argocd --request-timeout=5s >/dev/null 2>&1; do
    if (( SECONDS > _t )); then
        info "  argocd namespace still present — forcing finalize again..."
        kubectl get namespace argocd -o json --request-timeout=10s 2>/dev/null \
            | jq '.spec.finalizers = []' \
            | kubectl replace --raw "/api/v1/namespaces/argocd/finalize" -f - \
            2>/dev/null || true
        sleep 5
        break
    fi
    sleep 2
done
success "ArgoCD namespace removed."

# =============================================================================
# Step 7 — Uninstall External Secrets Operator
# =============================================================================
banner "Step 7 — Uninstall External Secrets Operator"

if helm status external-secrets -n external-secrets >/dev/null 2>&1; then
    helm uninstall external-secrets -n external-secrets
    success "ESO Helm release uninstalled."
fi
kubectl delete namespace external-secrets --ignore-not-found=true 2>/dev/null || true

# Strip finalizers from all ESO CRs before deleting CRDs.
# If the CRD is deleted while CR instances still carry finalizers the API
# server blocks indefinitely waiting for a controller that no longer exists.
for eso_type in \
    externalsecrets.external-secrets.io \
    clusterexternalsecrets.external-secrets.io \
    secretstores.external-secrets.io \
    clustersecretstores.external-secrets.io \
    pushsecrets.external-secrets.io; do
    kubectl get "${eso_type}" --all-namespaces -o json 2>/dev/null \
        | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name)"' \
        | while IFS=/ read -r ns name; do
            kubectl patch "${eso_type}" "${name}" \
                ${ns:+-n "${ns}"} \
                --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                2>/dev/null || true
        done
done

# Delete CRDs without waiting so we don't block on GC.
kubectl get crd -o name 2>/dev/null | grep "external-secrets.io" \
    | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
success "ESO removed."

# =============================================================================
# Step 8 — Uninstall cert-manager (only if Gentian-managed)
# =============================================================================
if [[ "${UNINSTALL_CLUSTER_INFRA}" == "1" || "${GENTIAN_MANAGED_CERT_MANAGER}" == "1" ]]; then
    banner "Step 8 — Uninstall cert-manager"

    if helm status cert-manager -n cert-manager >/dev/null 2>&1; then
        helm uninstall cert-manager -n cert-manager
        kubectl delete namespace cert-manager --ignore-not-found=true 2>/dev/null || true
        # Strip finalizers from cert-manager CRs before deleting CRDs.
        for cm_type in \
            certificates.cert-manager.io \
            certificaterequests.cert-manager.io \
            issuers.cert-manager.io \
            clusterissuers.cert-manager.io \
            orders.acme.cert-manager.io \
            challenges.acme.cert-manager.io; do
            kubectl get "${cm_type}" --all-namespaces -o json 2>/dev/null \
                | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name)"' \
                | while IFS=/ read -r ns name; do
                    kubectl patch "${cm_type}" "${name}" \
                        ${ns:+-n "${ns}"} \
                        --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                        2>/dev/null || true
                done
        done
        kubectl get crd -o name 2>/dev/null | grep cert-manager.io \
            | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
        success "cert-manager removed."
    else
        info "cert-manager has no Helm release — skipping (likely managed outside Gentian, e.g. microk8s addon)."
    fi
else
    info "Skipping cert-manager removal (not Gentian-managed; use --cluster-infra to force)."
fi

# =============================================================================
# Step 9 — Remove remaining kernel namespaces
# In safe mode, namespaces that contain PVCs are preserved.
# In force mode, all kernel namespaces are deleted.
# =============================================================================
banner "Step 9 — Remove kernel namespaces"

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
    kubectl delete namespace "${ns}" --ignore-not-found=true --grace-period=5 2>/dev/null || true
    # Wait up to 30s; if still Terminating, strip finalizers and retry.
    local deadline=$((SECONDS + 30))
    while kubectl get namespace "${ns}" >/dev/null 2>&1; do
        if (( SECONDS > deadline )); then
            warn "  ${ns} stuck in Terminating — stripping remaining finalizers..."
            _clear_ns_finalizers "${ns}"
            # Also clear the namespace-level finalizer itself.
            kubectl get namespace "${ns}" -o json 2>/dev/null \
              | jq '.spec.finalizers=[]' \
              | kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - 2>/dev/null || true
            break
        fi
        sleep 3
    done
    if kubectl get namespace "${ns}" >/dev/null 2>&1; then
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
#      bind to the surviving old PVCs and inherit old data (e.g. LDAP users).
# Helm uninstall intentionally preserves StatefulSet volumeClaimTemplate PVCs.
_drain_pvcs() {
    local ns="$1"
    local pvcs
    pvcs=$(kubectl get pvc -n "${ns}" -o name 2>/dev/null) || return 0
    [[ -z "${pvcs}" ]] && return 0

    info "  Draining PVCs in ${ns} (waiting for pods to terminate first)..."
    # Wait up to 90s for all pods to exit — the pvc-protection controller
    # clears the kubernetes.io/pvc-protection finalizer only once no pod
    # mounts the PVC. Without this wait, kubectl delete pvc hangs.
    kubectl wait pod --all -n "${ns}" --for=delete --timeout=90s 2>/dev/null || true

    info "  Deleting PVCs in ${ns}..."
    kubectl delete pvc --all -n "${ns}" --wait=true --timeout=120s 2>/dev/null || true

    # Belt-and-suspenders: strip any lingering pvc-protection finalizers so
    # that a timed-out delete doesn't block namespace termination.
    kubectl get pvc -n "${ns}" -o name 2>/dev/null \
        | xargs -r -I%% kubectl patch %% -n "${ns}" \
            --type=merge -p='{"metadata":{"finalizers":[]}}' \
            2>/dev/null || true
}

for ns in openbao gentian-dev gentian-infra-dev gentian-system platform-kernel tofu-system; do
    if ! kubectl get namespace "${ns}" >/dev/null 2>&1; then
        continue
    fi
    if [[ "${MODE}" == "force" ]]; then
        _drain_pvcs "${ns}"
        _delete_namespace "${ns}"
    elif _has_pvc "${ns}"; then
        warn "  Skipping namespace ${ns} (contains PVCs — use -f to delete)"
    else
        _delete_namespace "${ns}"
    fi
done

# =============================================================================
# Done
# =============================================================================
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║     Gentian OS — Uninstall complete                      ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""
echo "  OpenBao KV data is PRESERVED (managementPolicies: Observe/Create"
echo "  prevents Crossplane from deleting KV paths on XR deletion)."
echo ""
if [[ "${MODE}" == "safe" ]]; then
    echo "  PVC/PV data is preserved (safe mode)."
    echo "  Re-run with -f to also remove namespaces and bound PVs."
fi
echo ""
echo "  To re-install: ./install-cp.sh"
echo ""
