#!/usr/bin/env bash
# =============================================================================
# uninstall.sh — Gentian OS uninstall helper
# =============================================================================
# Default mode: safe
#   - Removes Gentian GitOps/bootstrap controllers and Applications
#   - Keeps persistent storage resources (PVC/PV) and namespaces that contain PVCs
#
# Force mode: -f
#   - Performs safe uninstall steps
#   - Also deletes Gentian data namespaces and bound PVs for full teardown
#
# Usage:
#   ./uninstall.sh
#   ./uninstall.sh -f
#   ./uninstall.sh --cluster-infra
#   ./uninstall.sh -f --cluster-infra
# =============================================================================

set -euo pipefail

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }

MODE="safe"
UNINSTALL_CLUSTER_INFRA=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        -f)
            MODE="force"
            ;;
        --cluster-infra)
            UNINSTALL_CLUSTER_INFRA=1
            ;;
        --no-cluster-infra)
            UNINSTALL_CLUSTER_INFRA=0
            ;;
        -h|--help)
            echo "Usage: $0 [-f] [--cluster-infra]"
            echo "  default: safe uninstall (preserve PVC/PV state)"
            echo "  -f               : force uninstall (delete namespaces + bound PVs)"
            echo "  --cluster-infra  : also uninstall cluster infra (cert-manager/reloader/cnpg)"
            echo "  --no-cluster-infra: skip cluster infra uninstall (default)"
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
    if ! command -v "$cmd" >/dev/null 2>&1; then
        error "Missing required command: $cmd"
        exit 1
    fi
done

APP_NS="argocd"

BOOTSTRAP_APPS_CORE=(
    gentian-appsets
    openbao-transit
    openbao
    tofu-controller
    gentian-globals-cluster
)

BOOTSTRAP_APPS_INFRA=(
    reloader
    cnpg
    cnpg-cluster-dev
)

APPSETS=(
    external-secrets
    gentian-tofu
    gentian-infra
    gentian-iam
    gentian-nubus
    gentian-kernel-services
    gentian-apps
)

TARGET_NAMESPACES_CORE=(
    argocd
    external-secrets
    tofu-system
    gentian-system
    openbao
    platform-kernel
    gentian-dev
    gentian-infra-dev
)

TARGET_NAMESPACES_INFRA=(
    cert-manager
    stakater-system
    cnpg-system
)

delete_app_safely() {
    local name="$1"
    if kubectl get application "$name" -n "$APP_NS" >/dev/null 2>&1; then
        kubectl patch application "$name" -n "$APP_NS" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        kubectl delete application "$name" -n "$APP_NS" --ignore-not-found >/dev/null 2>&1 || true
        success "Deleted Application $name"
    fi
}

delete_app_force() {
    local name="$1"
    kubectl delete application "$name" -n "$APP_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

delete_appset_safely() {
    local name="$1"
    if kubectl get applicationset "$name" -n "$APP_NS" >/dev/null 2>&1; then
        kubectl patch applicationset "$name" -n "$APP_NS" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        kubectl delete applicationset "$name" -n "$APP_NS" --ignore-not-found >/dev/null 2>&1 || true
        success "Deleted ApplicationSet $name"
    fi
}

delete_appset_force() {
    local name="$1"
    kubectl delete applicationset "$name" -n "$APP_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

namespace_has_pvc() {
    local ns="$1"
    local count
    count=$(kubectl get pvc -n "$ns" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    [[ "$count" != "0" ]]
}

banner_msg="Gentian OS uninstall (${MODE})"
echo ""
echo -e "${CYAN}============================================================${NC}"
echo -e "${CYAN}${banner_msg}${NC}"
echo -e "${CYAN}============================================================${NC}"
echo ""

info "Removing Gentian bootstrap applications from ArgoCD..."
for app in "${BOOTSTRAP_APPS_CORE[@]}"; do
    if [[ "$MODE" == "safe" ]]; then
        delete_app_safely "$app"
    else
        delete_app_force "$app"
    fi
done

if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
    info "Removing cluster infra bootstrap applications from ArgoCD..."
    for app in "${BOOTSTRAP_APPS_INFRA[@]}"; do
        if [[ "$MODE" == "safe" ]]; then
            delete_app_safely "$app"
        else
            delete_app_force "$app"
        fi
    done
else
    warn "Keeping cluster infra bootstrap Applications (use --cluster-infra to remove)."
fi

info "Removing Gentian ApplicationSets..."
for appset in "${APPSETS[@]}"; do
    if [[ "$MODE" == "safe" ]]; then
        delete_appset_safely "$appset"
    else
        delete_appset_force "$appset"
    fi
done

info "Removing Gentian AppProject..."
kubectl patch appproject gentian -n "$APP_NS" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
kubectl delete appproject gentian -n "$APP_NS" --ignore-not-found >/dev/null 2>&1 || true

info "Uninstalling Helm releases installed directly by install.sh..."
helm uninstall external-secrets -n external-secrets >/dev/null 2>&1 || true
if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
    helm uninstall cert-manager -n cert-manager >/dev/null 2>&1 || true
fi

if [[ "$MODE" == "safe" ]]; then
    info "Safe mode: deleting only non-data namespaces. Namespaces with PVCs are preserved."
    local_namespaces=("${TARGET_NAMESPACES_CORE[@]}")
    if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
        local_namespaces+=("${TARGET_NAMESPACES_INFRA[@]}")
    fi
    for ns in "${local_namespaces[@]}"; do
        if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
            continue
        fi
        if namespace_has_pvc "$ns"; then
            warn "Keeping namespace $ns (contains PVCs)."
            continue
        fi
        kubectl delete namespace "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
        success "Deleted namespace $ns"
    done
    warn "Safe uninstall complete. Persistent data namespaces/PVCs were preserved."
else
    info "Force mode: deleting all Gentian namespaces and bound PVs."
    local_namespaces=("${TARGET_NAMESPACES_CORE[@]}")
    if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
        local_namespaces+=("${TARGET_NAMESPACES_INFRA[@]}")
    fi
    for ns in "${local_namespaces[@]}"; do
        kubectl delete namespace "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done

    if command -v jq >/dev/null 2>&1; then
                if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
                        mapfile -t pvs < <(kubectl get pv -o json | jq -r '.items[]
                            | select(.spec.claimRef != null)
                            | select(["argocd","external-secrets","tofu-system","gentian-system","openbao","platform-kernel","gentian-dev","gentian-infra-dev","cert-manager","stakater-system","cnpg-system"]
                                | index(.spec.claimRef.namespace))
                            | .metadata.name')
                else
                        mapfile -t pvs < <(kubectl get pv -o json | jq -r '.items[]
                            | select(.spec.claimRef != null)
                            | select(["argocd","external-secrets","tofu-system","gentian-system","openbao","platform-kernel","gentian-dev","gentian-infra-dev"]
                                | index(.spec.claimRef.namespace))
                            | .metadata.name')
                fi
        if [[ ${#pvs[@]} -gt 0 ]]; then
            kubectl delete pv "${pvs[@]}" --ignore-not-found >/dev/null 2>&1 || true
            success "Deleted bound PVs for Gentian namespaces."
        fi
    else
        warn "jq not found; skipped targeted PV deletion."
    fi

    # Remove Gentian app CRDs created by install/bootstrap.
    kubectl delete crd gentianos.io_appcatalogues gentianos.io_appprofiles gentianos.io_integrationbindings gentianos.io_tenants --ignore-not-found >/dev/null 2>&1 || true
    success "Force uninstall completed."
fi

echo ""
info "Done."
