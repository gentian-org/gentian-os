#!/usr/bin/env bash
# =============================================================================
# uninstall-cp.sh — Gentian OS Crossplane-based uninstall
# =============================================================================
# Reverses install-cp.sh (Phase 1 bootstrap) in reverse order.
#
# Default (safe) mode:
#   - Removes the Cluster XR (waits for Crossplane GC)
#   - Removes Crossplane resources (XRD, Composition, providers)
#   - Uninstalls Crossplane core
#   - Removes ArgoCD, ESO, cert-manager
#   - Preserves PVC/PV data and namespaces that contain PVCs
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
# Step 2 — Remove Crossplane compositions, XRDs, and ProviderConfigs
# =============================================================================
banner "Step 2 — Remove Crossplane XRDs, Compositions, ProviderConfigs"

for file in \
    "${SCRIPT_DIR}/crossplane/compositions/cluster-default.yaml" \
    "${SCRIPT_DIR}/crossplane/xrds/cluster.yaml" \
    "${SCRIPT_DIR}/crossplane/providers/provider-configs.yaml"
do
    if [[ -f "${file}" ]]; then
        kubectl delete -f "${file}" --ignore-not-found=true
        success "  Removed: $(basename "${file}")"
    fi
done

# =============================================================================
# Step 3 — Uninstall Crossplane providers
# =============================================================================
banner "Step 3 — Uninstall Crossplane providers"

for provider in function-go-templating provider-kubernetes provider-vault; do
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
    gentian-os-kernel-mail-postfix
do
    kubectl delete secret "${secret}" -n crossplane-system \
        --ignore-not-found=true 2>/dev/null || true
done
success "Derived-credential Secrets removed."

# =============================================================================
# Step 6 — Uninstall ArgoCD
# Strip Application finalizers first: when the ArgoCD API server is deleted
# its finalizer handler disappears, leaving Applications stuck in Terminating
# and blocking namespace deletion indefinitely.
# =============================================================================
banner "Step 6 — Uninstall ArgoCD"

# Remove resources-finalizer.argocd.argoproj.io from all Applications so the
# namespace can terminate cleanly even after the ArgoCD server is gone.
if kubectl get crd applications.argoproj.io >/dev/null 2>&1; then
    info "Stripping ArgoCD Application finalizers..."
    kubectl get applications.argoproj.io -n argocd -o name 2>/dev/null \
        | xargs -r -I{} kubectl patch {} -n argocd \
            --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
            2>/dev/null || true
    success "  Application finalizers cleared."
fi

if helm status argocd -n argocd >/dev/null 2>&1; then
    helm uninstall argocd -n argocd
    success "ArgoCD Helm release uninstalled."
else
    # Non-Helm install: delete namespace directly.
    kubectl delete namespace argocd --ignore-not-found=true || true
    success "ArgoCD namespace removed."
fi

# Remove ArgoCD CRDs (safe since no Applications were deployed in Phase 1).
kubectl get crd -o name 2>/dev/null | grep argoproj.io \
    | xargs -r kubectl delete --ignore-not-found=true 2>/dev/null || true
success "ArgoCD CRDs removed."

# =============================================================================
# Step 7 — Uninstall External Secrets Operator
# =============================================================================
banner "Step 7 — Uninstall External Secrets Operator"

if helm status external-secrets -n external-secrets >/dev/null 2>&1; then
    helm uninstall external-secrets -n external-secrets
    success "ESO Helm release uninstalled."
fi
kubectl delete namespace external-secrets --ignore-not-found=true 2>/dev/null || true
kubectl get crd -o name 2>/dev/null | grep external-secrets.io \
    | xargs -r kubectl delete --ignore-not-found=true 2>/dev/null || true
success "ESO removed."

# =============================================================================
# Step 8 — Uninstall cert-manager (only if Gentian-managed)
# =============================================================================
if [[ "${UNINSTALL_CLUSTER_INFRA}" == "1" || "${GENTIAN_MANAGED_CERT_MANAGER}" == "1" ]]; then
    banner "Step 8 — Uninstall cert-manager"

    if helm status cert-manager -n cert-manager >/dev/null 2>&1; then
        helm uninstall cert-manager -n cert-manager
        success "cert-manager Helm release uninstalled."
    fi
    kubectl delete namespace cert-manager --ignore-not-found=true 2>/dev/null || true
    kubectl get crd -o name 2>/dev/null | grep cert-manager.io \
        | xargs -r kubectl delete --ignore-not-found=true 2>/dev/null || true
    success "cert-manager removed."
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

for ns in openbao gentian-system platform-kernel tofu-system; do
    if ! kubectl get namespace "${ns}" >/dev/null 2>&1; then
        continue
    fi
    if [[ "${MODE}" == "force" ]]; then
        kubectl delete namespace "${ns}" --ignore-not-found=true \
            --grace-period=5 2>/dev/null || true
        success "  Deleted namespace: ${ns}"
    elif _has_pvc "${ns}"; then
        warn "  Skipping namespace ${ns} (contains PVCs — use -f to delete)"
    else
        kubectl delete namespace "${ns}" --ignore-not-found=true \
            --grace-period=5 2>/dev/null || true
        success "  Deleted namespace: ${ns}"
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
