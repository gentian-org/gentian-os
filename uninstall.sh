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
UNINSTALL_CERT_MANAGER=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_STATE_FILE="${INSTALL_STATE_FILE:-${SCRIPT_DIR}/.install-state.env}"
GENTIAN_MANAGED_CERT_MANAGER="${GENTIAN_MANAGED_CERT_MANAGER:-0}"

# Shared CRI / kubelet runtime helpers (ensure_sudo, cri_cleanup,
# kubelite_restart). Same implementation install.sh uses.
# shellcheck source=scripts/lib-runtime.sh
source "${SCRIPT_DIR}/scripts/lib-runtime.sh"

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

# Load installer state (if present) so we only uninstall cert-manager when it
# was actually managed by install.sh.
if [[ -r "${INSTALL_STATE_FILE}" ]]; then
    # shellcheck disable=SC1090
    source "${INSTALL_STATE_FILE}" || true
fi

if [[ "${UNINSTALL_CLUSTER_INFRA}" == "1" ]]; then
    if [[ "${GENTIAN_MANAGED_CERT_MANAGER:-0}" == "1" ]]; then
        UNINSTALL_CERT_MANAGER=1
    else
        warn "Installer state indicates cert-manager is NOT managed by gentian-os;"
        warn "keeping cert-manager in place while removing other Gentian infra."
    fi
fi

APP_NS="argocd"

# =============================================================================
# Step 0 — Reset gentian-deployments: remove all deployed tenant instances
# =============================================================================
# Done before any ArgoCD / Helm teardown so the deployments repo is clean when
# install.sh runs next time. We edit the kustomization files directly (rather
# than calling `kubectl gentian tenants undeploy`) because the plugin calls
# `kubectl apply -k` after the git commit, which would fail on a
# partially-torn-down cluster.
undeploy_all_tenants() {
    local config_file="${HOME}/.gentian/config"
    local deployments_path=""

    if [[ -f "${config_file}" ]]; then
        # shellcheck disable=SC1090
        source "${config_file}" 2>/dev/null || true
    fi
    deployments_path="${GENTIAN_DEPLOYMENTS_PATH:-}"

    if [[ -z "${deployments_path}" ]]; then
        warn "GENTIAN_DEPLOYMENTS_PATH not set; skipping tenant kustomization cleanup."
        return 0
    fi
    if [[ ! -d "${deployments_path}/.git" ]]; then
        warn "${deployments_path} is not a git repo; skipping tenant kustomization cleanup."
        return 0
    fi

    local changed=0
    # Find every tenants/kustomization.yaml across all envs (dev, staging, prod, …)
    while IFS= read -r kustom_file; do
        local env_name
        env_name=$(basename "$(dirname "$(dirname "${kustom_file}")")")

        # Extract instance names from lines like:  - instances/<name>
        local instances=()
        mapfile -t instances < <(
            grep -oE '[[:space:]]*-[[:space:]]+instances/([^[:space:]]+)' "${kustom_file}" \
                | sed 's|.*instances/||' 2>/dev/null || true
        )

        for instance in "${instances[@]}"; do
            [[ -z "${instance}" ]] && continue
            info "  Removing ${instance} from ${env_name} (kustomization cleanup)..."
            # Delete the exact resource line; handles optional surrounding whitespace
            sed -i "/^[[:space:]]*-[[:space:]]*instances\/${instance}[[:space:]]*$/d" "${kustom_file}"
            changed=1
        done
    done < <(find "${deployments_path}" -path "*/tenants/kustomization.yaml" 2>/dev/null)

    if [[ "${changed}" -eq 0 ]]; then
        success "Deployments repo already clean (no deployed tenant instances found)."
        return 0
    fi

    (
        cd "${deployments_path}"
        git add -A
        if git diff --cached --quiet; then
            info "No deployments-repo changes to commit."
        else
            git commit -m "chore: undeploy all tenants (uninstall.sh cleanup)"
            git push || warn "git push to deployments repo failed; commit is local only."
            success "Tenant instances removed from deployments repo."
        fi
    )
}

info "Cleaning deployments repo: undeploying all tenant instances..."
undeploy_all_tenants

BOOTSTRAP_APPS_CORE=(
    gentian-appsets
    gentian-appprofiles
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
    flux-system
    gentian-system
    openbao
    platform-kernel
    gentian-dev
    gentian-infra-dev
)

TARGET_NAMESPACES_INFRA=(
    stakater-system
    cnpg-system
)
if [[ "${UNINSTALL_CERT_MANAGER}" == "1" ]]; then
    TARGET_NAMESPACES_INFRA=(cert-manager "${TARGET_NAMESPACES_INFRA[@]}")
fi

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

# ─── KERNEL TLS / WILDCARD CLEANUP ──────────────────────────────────────────
# Resources created by install_kernel_cert_resources / install_kernel_wildcard
# live in the cert-manager namespace (which is only torn down with
# --cluster-infra) and at cluster scope (ClusterIssuers). They must be cleaned
# regardless of --cluster-infra, otherwise a re-install will:
#   • inherit a stale wildcard-kernel Certificate referencing a deleted Issuer
#   • leak _acme-challenge TXT records on Cloudflare (each retry adds another)
#   • leave orphan Order/Challenge CRs that block fresh ACME orders with the
#     "DELETE /zones//dns_records/<id>" empty-zone-id bug we hit twice during
#     development.
# This section is best-effort: every step swallows errors so the rest of the
# uninstall can proceed even when cert-manager is half-torn-down.
# ─────────────────────────────────────────────────────────────────────────────
cleanup_kernel_tls() {
    info "Cleaning up kernel TLS / ACME state..."

    # Try to recover the Cloudflare token (in priority order) so we can also
    # purge stale _acme-challenge TXT records from the Cloudflare zone.
    local cf_token="" cf_zone="${CF_ZONE_NAME:-}"
    if [[ -n "${CF_API_TOKEN:-}" ]]; then
        cf_token="$CF_API_TOKEN"
    elif kubectl get secret cloudflare-api-token -n cert-manager >/dev/null 2>&1; then
        cf_token=$(kubectl get secret cloudflare-api-token -n cert-manager \
            -o jsonpath='{.data.api-token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    fi
    if [[ -z "$cf_zone" ]]; then
        # Derive from KERNEL_DOMAIN cached in .install-secrets.env if present.
        local cache="${BASH_SOURCE[0]%/*}/.install-secrets.env"
        if [[ -r "$cache" ]]; then
            local dom
            dom=$(awk -F'=' '/^KERNEL_DOMAIN=/{gsub(/"/,"",$2); print $2; exit}' "$cache")
            [[ -n "$dom" ]] && cf_zone=$(echo "$dom" | awk -F. '{n=NF; print $(n-1)"."$n}')
        fi
    fi

    # 1) Delete kernel wildcard Certificate + materialized Secret. Strip
    #    finalizer first so the delete is non-blocking even if cert-manager
    #    controller is already gone.
    if kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
        kubectl patch certificate wildcard-kernel -n cert-manager \
            --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        kubectl delete certificate wildcard-kernel -n cert-manager \
            --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    kubectl delete secret wildcard-kernel-tls -n cert-manager \
        --ignore-not-found >/dev/null 2>&1 || true

    # 2) Delete Cloudflare ExternalSecret + materialized Secret. Strip ESO
    #    finalizer first.
    if kubectl get crd externalsecrets.external-secrets.io >/dev/null 2>&1; then
        kubectl patch externalsecret cloudflare-api-token -n cert-manager \
            --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        kubectl delete externalsecret cloudflare-api-token -n cert-manager \
            --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    kubectl delete secret cloudflare-api-token -n cert-manager \
        --ignore-not-found >/dev/null 2>&1 || true

    # 3) Delete ALL ACME state (Orders, Challenges, CertificateRequests) in
    #    every namespace. These are the resources that get stuck in the
    #    "DELETE /zones//dns_records/<id>" cleanup loop when cert-manager
    #    loses zone_id from in-memory state across restarts. Strip
    #    finalizers first to make delete non-blocking.
    if kubectl get crd challenges.acme.cert-manager.io >/dev/null 2>&1; then
        for kind in challenge order certificaterequest; do
            while IFS=/ read -r ns name; do
                [[ -z "$ns" || -z "$name" ]] && continue
                kubectl patch "$kind" "$name" -n "$ns" --type=merge \
                    -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
                kubectl delete "$kind" "$name" -n "$ns" \
                    --ignore-not-found --wait=false >/dev/null 2>&1 || true
            done < <(kubectl get "$kind" -A \
                     -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null)
        done
    fi

    # 4) Delete the two ClusterIssuers created by install.sh step 3b.
    if kubectl get crd clusterissuers.cert-manager.io >/dev/null 2>&1; then
        kubectl delete clusterissuer letsencrypt-http01 letsencrypt-dns01-cloudflare \
            --ignore-not-found >/dev/null 2>&1 || true
    fi

    # 5) Best-effort: purge stale _acme-challenge.* TXT records on Cloudflare
    #    so the next install starts with a clean zone. Skipped silently when
    #    we couldn't recover a token or zone name.
    if [[ -n "$cf_token" && -n "$cf_zone" ]]; then
        if ! command -v jq >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
            warn "Skipping Cloudflare TXT cleanup (jq/curl missing)."
        else
            info "Purging stale _acme-challenge.* TXT records in zone ${cf_zone}..."
            local zid
            zid=$(curl -sS --max-time 10 -H "Authorization: Bearer ${cf_token}" \
                "https://api.cloudflare.com/client/v4/zones?name=${cf_zone}" \
                | jq -r '.result[0].id // empty' 2>/dev/null)
            if [[ -z "$zid" ]]; then
                warn "Couldn't resolve zone id for ${cf_zone}; skipping TXT cleanup."
            else
                local deleted=0 rid
                while read -r rid; do
                    [[ -z "$rid" ]] && continue
                    if curl -sS --max-time 10 -X DELETE \
                        -H "Authorization: Bearer ${cf_token}" \
                        "https://api.cloudflare.com/client/v4/zones/${zid}/dns_records/${rid}" \
                        | jq -e '.success' >/dev/null 2>&1; then
                        deleted=$((deleted + 1))
                    fi
                done < <(curl -sS --max-time 10 -H "Authorization: Bearer ${cf_token}" \
                    "https://api.cloudflare.com/client/v4/zones/${zid}/dns_records?type=TXT&per_page=100" \
                    | jq -r '.result[]? | select(.name | startswith("_acme-challenge")) | .id' 2>/dev/null)
                if [[ $deleted -gt 0 ]]; then
                    success "Deleted ${deleted} stale _acme-challenge TXT record(s) on Cloudflare."
                else
                    info "No stale _acme-challenge TXT records found in zone ${cf_zone}."
                fi
            fi
        fi
    else
        warn "Cloudflare token or zone unknown; skipping remote TXT cleanup."
        warn "  → after re-install, watch out for residual _acme-challenge.* records."
    fi

    success "Kernel TLS / ACME state cleanup complete."
}
cleanup_kernel_tls

# Wipe install-time caches on force uninstall so a follow-up install prompts
# cleanly (including KERNEL_DOMAIN). In safe mode we keep caches so the user
# can re-install without re-typing every value.
if [[ "$MODE" == "force" ]]; then
    _secrets_cache="${BASH_SOURCE[0]%/*}/.install-secrets.env"
    _state_cache="${BASH_SOURCE[0]%/*}/.install-state.env"
    if [[ -e "$_secrets_cache" ]]; then
        rm -f "$_secrets_cache"
        success "Removed install-time credential cache ${_secrets_cache}."
    fi
    if [[ -e "$_state_cache" ]]; then
        rm -f "$_state_cache"
        success "Removed install-time state cache ${_state_cache}."
    fi

    # Remove OpenBao init files from /tmp so a follow-up install re-prompts
    # for the transit and primary tokens. These files survive namespace
    # deletion because they are written to the host filesystem. Without
    # clearing them, openbao-transit (which uses a microk8s-hostpath PVC whose
    # underlying directory persists across reinstalls) silently reuses the old
    # token from the file — bypassing the "save to Bitwarden" prompts entirely.
    _transit_init="${TRANSIT_INIT_FILE:-/tmp/openbao-transit-init.json}"
    _primary_init="${OPENBAO_INIT_FILE:-/tmp/openbao-init.json}"
    for _f in "$_transit_init" "$_primary_init"; do
        if [[ -e "$_f" ]]; then
            rm -f "$_f"
            success "Removed OpenBao init cache ${_f}."
        fi
    done
fi

info "Uninstalling Helm releases installed directly by install.sh..."
helm uninstall external-secrets -n external-secrets >/dev/null 2>&1 || true
helm uninstall gentian-os -n gentian-system >/dev/null 2>&1 || true
if [[ "$UNINSTALL_CERT_MANAGER" == "1" ]]; then
    helm uninstall cert-manager -n cert-manager >/dev/null 2>&1 || true
fi

# Delete workload controllers in target namespaces BEFORE deleting the
# namespaces themselves. Otherwise the namespace deletion can race ahead and
# leave orphaned Deployments/ReplicaSets behind in api-server. Those orphans
# then keep the replicaset-controller logging "namespace not found" errors,
# which jams kubelet's event sync loop on the next install — pods get stuck
# in ContainerCreating with calico IP assigned but PodReadyToStartContainers
# never flips to True.
info "Deleting workload controllers in target namespaces (prevents orphan zombies)..."
_pre_delete_namespaces=("${TARGET_NAMESPACES_CORE[@]}")
if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
    _pre_delete_namespaces+=("${TARGET_NAMESPACES_INFRA[@]}")
fi
for ns in "${_pre_delete_namespaces[@]}"; do
    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        continue
    fi
    kubectl delete deploy,sts,ds,rs,job,cronjob -n "$ns" --all \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
done

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
    KUBECTL_REQUEST_TIMEOUT="${KUBECTL_REQUEST_TIMEOUT:-10s}"
    KUBECTL_HARD_TIMEOUT="${KUBECTL_HARD_TIMEOUT:-15s}"
    _kctl() {
        if command -v timeout >/dev/null 2>&1; then
            timeout --foreground "$KUBECTL_HARD_TIMEOUT" \
                kubectl --request-timeout="$KUBECTL_REQUEST_TIMEOUT" "$@"
        else
            kubectl --request-timeout="$KUBECTL_REQUEST_TIMEOUT" "$@"
        fi
    }
    local_namespaces=("${TARGET_NAMESPACES_CORE[@]}")
    if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
        local_namespaces+=("${TARGET_NAMESPACES_INFRA[@]}")
    fi

    # ── PRE-DELETE FINALIZER STRIP ──────────────────────────────────────────
    # The two recurring causes of stuck-Terminating namespaces are:
    #   1. argocd: Application CRs carry resources-finalizer.argocd.argoproj.io.
    #      Once the argocd controller deployment is gone (helm uninstall above),
    #      nothing removes those finalizers, so the namespace can never
    #      garbage-collect them.
    #   2. openbao: StatefulSet PVCs carry kubernetes.io/pvc-protection. If the
    #      pod can't terminate cleanly (CNI hang) the finalizer never clears
    #      and the namespace blocks on its dependent PVCs.
    # Strip both BEFORE issuing the namespace delete so the namespace
    # controller has nothing left to wait on.
    info "Pre-stripping finalizers on Apps / AppSets / Terraforms / PVCs..."
    info "  - ArgoCD Applications"
    if _kctl get crd applications.argoproj.io >/dev/null 2>&1; then
        mapfile -t _apps < <(_kctl get application -n "$APP_NS" -o name 2>/dev/null || true)
        for app in "${_apps[@]}"; do
            [[ -n "$app" ]] || continue
            _kctl patch -n "$APP_NS" "$app" --type=merge \
                -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
            _kctl delete -n "$APP_NS" "$app" --wait=false --ignore-not-found >/dev/null 2>&1 || true
        done
    fi
    info "  - ArgoCD ApplicationSets"
    if _kctl get crd applicationsets.argoproj.io >/dev/null 2>&1; then
        mapfile -t _appsets < <(_kctl get applicationset -n "$APP_NS" -o name 2>/dev/null || true)
        for as in "${_appsets[@]}"; do
            [[ -n "$as" ]] || continue
            _kctl patch -n "$APP_NS" "$as" --type=merge \
                -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
            _kctl delete -n "$APP_NS" "$as" --wait=false --ignore-not-found >/dev/null 2>&1 || true
        done
    fi
    info "  - Tofu Terraform CRs"
    if _kctl get crd terraforms.infra.contrib.fluxcd.io >/dev/null 2>&1; then
        while IFS=/ read -r ns name; do
            [[ -z "$ns" || -z "$name" ]] && continue
            _kctl patch terraform "$name" -n "$ns" --type=merge \
                -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
            _kctl delete terraform "$name" -n "$ns" --wait=false --ignore-not-found >/dev/null 2>&1 || true
        done < <(_kctl get terraform -A \
                 -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null)
    fi
    # Strip finalizers from CNPG Cluster CRs so that deleting the CNPG CRD
    # later does not block waiting for instances to finalize. CNPG adds its
    # own finalizer (cnpg.io/cluster) that the CRD controller must clear.
    if _kctl get crd clusters.postgresql.cnpg.io >/dev/null 2>&1; then
        while IFS=/ read -r ns name; do
            [[ -z "$ns" || -z "$name" ]] && continue
            _kctl patch cluster.postgresql.cnpg.io "$name" -n "$ns" --type=merge \
                -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
            _kctl delete cluster.postgresql.cnpg.io "$name" -n "$ns" --wait=false --ignore-not-found >/dev/null 2>&1 || true
        done < <(_kctl get cluster.postgresql.cnpg.io -A \
                 -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null)
    fi
    # Strip pvc-protection finalizers in all target namespaces up-front.
    info "  - PVC finalizers in target namespaces"
    for ns in "${local_namespaces[@]}"; do
        if ! _kctl get namespace "$ns" >/dev/null 2>&1; then continue; fi
        mapfile -t _pvcs < <(_kctl get pvc -n "$ns" -o name 2>/dev/null || true)
        for pvc in "${_pvcs[@]}"; do
            [[ -n "$pvc" ]] || continue
            _kctl patch -n "$ns" "$pvc" --type=merge \
                -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        done
    done

    # ── ISSUE NAMESPACE DELETES (non-blocking) ──────────────────────────────
    info "Issuing namespace deletes (non-blocking)..."
    for ns in "${local_namespaces[@]}"; do
        _kctl delete namespace "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done

    if command -v jq >/dev/null 2>&1; then
        info "Resolving bound PVs for deleted namespaces..."
        if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
            mapfile -t pvs < <(_kctl get pv -o json 2>/dev/null | jq -r '.items[]
                            | select(.spec.claimRef != null)
                            | .spec.claimRef.namespace as $ns
                            | select(["argocd","external-secrets","tofu-system","flux-system","gentian-system","openbao","platform-kernel","gentian-dev","gentian-infra-dev","cert-manager","stakater-system","cnpg-system"]
                                | index($ns))
                            | .metadata.name')
        else
            mapfile -t pvs < <(_kctl get pv -o json 2>/dev/null | jq -r '.items[]
                            | select(.spec.claimRef != null)
                            | .spec.claimRef.namespace as $ns
                            | select(["argocd","external-secrets","tofu-system","flux-system","gentian-system","openbao","platform-kernel","gentian-dev","gentian-infra-dev"]
                                | index($ns))
                            | .metadata.name')
        fi
        if [[ ${#pvs[@]} -gt 0 ]]; then
            info "Stripping PV finalizers and deleting ${#pvs[@]} PV(s)..."
            # Strip pv-protection finalizers first so delete is non-blocking.
            # Without this, `kubectl delete pv` blocks waiting for the volume
            # to detach from the node, which can take minutes (or forever if
            # the kubelet is wedged).
            for pv in "${pvs[@]}"; do
                _kctl patch pv "$pv" --type=merge \
                    -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
            done
            _kctl delete pv "${pvs[@]}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
            success "Deleted bound PVs for Gentian namespaces."
        else
            info "No bound PVs found for Gentian namespaces."
        fi
    else
        warn "jq not found; skipped targeted PV deletion."
    fi

    # Remove Gentian app CRDs created by install/bootstrap. Note: k8s CRD names
    # are <plural>.<group>, NOT the filename style <group>_<plural>.
    kubectl delete crd \
        appcatalogues.gentianos.io \
        appprofiles.gentianos.io \
        integrationbindings.gentianos.io \
        tenants.gentianos.io \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true

    # Remove ArgoCD CRDs (installed by the argocd helm chart in step 4).
    kubectl delete crd \
        applications.argoproj.io \
        applicationsets.argoproj.io \
        appprojects.argoproj.io \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true

    # Cluster-scoped infra cleanup (only with --cluster-infra). These
    # resources are not bound to any namespace, so they don't go away when
    # the helm release / argocd application is uninstalled. cert-manager is
    # NOT touched here because it predates gentian-os on most clusters.
    if [[ "$UNINSTALL_CLUSTER_INFRA" == "1" ]]; then
        info "Removing cluster-scoped infra CRDs and RBAC..."
        # CNPG (CloudNativePG) — finalizers stripped above in pre-strip phase
        kubectl delete crd \
            backups.postgresql.cnpg.io \
            clusterimagecatalogs.postgresql.cnpg.io \
            clusters.postgresql.cnpg.io \
            databases.postgresql.cnpg.io \
            imagecatalogs.postgresql.cnpg.io \
            poolers.postgresql.cnpg.io \
            publications.postgresql.cnpg.io \
            scheduledbackups.postgresql.cnpg.io \
            subscriptions.postgresql.cnpg.io \
            --ignore-not-found --wait=false >/dev/null 2>&1 || true
        # tofu-controller + flux source-controller — Terraform CRs stripped above
        kubectl delete crd \
            terraforms.infra.contrib.fluxcd.io \
            buckets.source.toolkit.fluxcd.io \
            externalartifacts.source.toolkit.fluxcd.io \
            gitrepositories.source.toolkit.fluxcd.io \
            helmcharts.source.toolkit.fluxcd.io \
            helmrepositories.source.toolkit.fluxcd.io \
            ocirepositories.source.toolkit.fluxcd.io \
            --ignore-not-found --wait=false >/dev/null 2>&1 || true
        # ClusterRoles and ClusterRoleBindings left behind when their
        # namespaces were force-deleted before the helm/argocd uninstall.
        kubectl delete clusterrole \
            gentian-os \
            argocd-application-controller \
            argocd-applicationset-controller \
            argocd-server \
            cnpg-cloudnative-pg \
            cnpg-cloudnative-pg-edit \
            cnpg-cloudnative-pg-view \
            tofu-cluster-reconciler-role \
            tofu-manager-role \
            reloader-reloader-role \
            --ignore-not-found >/dev/null 2>&1 || true
        kubectl delete clusterrolebinding \
            gentian-os \
            argocd-application-controller \
            argocd-applicationset-controller \
            argocd-server \
            cnpg-cloudnative-pg \
            tofu-cluster-reconciler \
            tofu-manager-rolebinding \
            reloader-reloader-role-binding \
            openbao-server-binding \
            openbao-transit-server-binding \
            --ignore-not-found >/dev/null 2>&1 || true
        success "Removed cluster-scoped infra CRDs and RBAC."
    fi

    # Force-delete any pods/PVCs still stuck in Terminating in target
    # namespaces. This handles container runtime / CNI sandbox teardown issues where
    # kubelet can't tear down a pod's network sandbox, which in turn keeps
    # the kubernetes.io/pvc-protection finalizer on its PVCs and prevents
    # the namespace from terminating.
    #
    # IMPORTANT: We try a graceful delete first (10s) so kubelet has a chance
    # to instruct the CRI runtime to tear down each pod's sandbox. Only the
    # genuinely-stuck pods get the --grace-period=0 --force escalation. The
    # naive "force everything" approach is what was leaking orphan containerd
    # sandboxes between install/uninstall cycles and wedging the next install
    # in ContainerCreating with the well-known status-sync hang.
    for ns in "${local_namespaces[@]}"; do
        if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
            continue
        fi
        # Phase 1: graceful delete (10s) so containerd can tear down sandboxes.
        for pod in $(kubectl get pods -n "$ns" -o name 2>/dev/null); do
            kubectl delete -n "$ns" "$pod" --grace-period=10 --timeout=15s --ignore-not-found >/dev/null 2>&1 || true
        done
        # Phase 2: force-delete only what's still left.
        for pod in $(kubectl get pods -n "$ns" -o name 2>/dev/null); do
            kubectl delete -n "$ns" "$pod" --grace-period=0 --force --ignore-not-found >/dev/null 2>&1 || true
        done
        # Strip pvc-protection finalizers and force-delete PVCs.
        for pvc in $(kubectl get pvc -n "$ns" -o name 2>/dev/null); do
            kubectl patch -n "$ns" "$pvc" -p '{"metadata":{"finalizers":[]}}' --type=merge >/dev/null 2>&1 || true
            kubectl delete -n "$ns" "$pvc" --grace-period=0 --force --ignore-not-found >/dev/null 2>&1 || true
        done
    done

    # If any target namespace is still Terminating, strip its kubernetes
    # finalizer so it can disappear. (This only runs after we've cleared the
    # CRs that were keeping it stuck.)
    for ns in "${local_namespaces[@]}"; do
        if kubectl get namespace "$ns" -o jsonpath='{.status.phase}' 2>/dev/null | grep -q Terminating; then
            kubectl get namespace "$ns" -o json 2>/dev/null \
                | jq '.spec.finalizers = []' \
                | kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - >/dev/null 2>&1 || true
        fi
    done

    success "Force uninstall completed."
fi

# =============================================================================
# Final runtime cleanup — sanity check, NOT a hammer
# =============================================================================
# The graceful-then-force pod sweep above already lets kubelet tear down
# CRI sandboxes properly, so in the happy path there's nothing to clean up
# here. We still run cri_cleanup as a defensive sanity check for the case
# where uninstall is run against an already-wedged cluster (kubelet was
# hanging *before* the operator typed ./uninstall.sh) — that's the only
# scenario where leaked sandboxes can survive the graceful sweep.
#
# Two deliberate constraints:
#   1) We do NOT call kubelite_restart here. Restarting kubelite at uninstall
#      makes the very next install hit a freshly-restarted kubelet, which
#      is exactly when the microk8s volume_manager race fires on the first
#      hostpath PVC pod (i.e. openbao-transit-0). Leaving kubelite alone
#      keeps the runtime warm; install.sh's PVC pre-warm absorbs any
#      residual race. If you genuinely need a kubelite restart, set
#      GENTIAN_FORCE_KUBELITE_RESTART=1.
#   2) cri_cleanup is best-effort and does NOT prompt for a sudo password.
#      If sudo isn't already cached (NOPASSWD or recent `sudo -v`), it
#      skips silently — sudo password prompts at uninstall time were the
#      original irritation that started this whole exercise.
#
# Skip the entire phase with GENTIAN_SKIP_RUNTIME_CLEANUP=1.
if [[ "${GENTIAN_SKIP_RUNTIME_CLEANUP:-0}" != "1" ]]; then
    echo ""
    if sudo -n true 2>/dev/null; then
        info "Final runtime sanity check (orphan sandbox sweep)..."
        cri_cleanup
    else
        info "Skipping CRI sandbox sweep (no cached sudo). Set"
        info "  GENTIAN_FORCE_KUBELITE_RESTART=1 or run \`sudo -v\` first if you"
        info "  need it (rarely necessary — graceful pod deletes above already"
        info "  released CRI state)."
    fi
    if [[ "${GENTIAN_FORCE_KUBELITE_RESTART:-0}" == "1" ]]; then
        kubelite_restart
    fi
fi

echo ""
info "Done."
