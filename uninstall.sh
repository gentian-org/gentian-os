#!/usr/bin/env bash
# =============================================================================
# uninstall.sh — Gentian OS Crossplane-based uninstall
# =============================================================================
# Reverses install.sh bootstrap in reverse order.
#
# Default (safe) mode:
#   - Undeploys all tenants (GitOps + in-cluster) unless --keep-tenants
#   - Removes kernel Pattern B Helm Release CRs and associated Secrets/ConfigMaps
#   - Removes the Cluster XR (waits for Crossplane GC)
#   - Removes Crossplane resources (XRD, Composition, providers)
#   - Tenant undeploy removes operator manifest-bridge ConfigMaps (tenant-*-provisioning-jobs)
#   - Uninstalls Crossplane core
#   - Removes ArgoCD, ESO, cert-manager
#   - Preserves PVC/PV data and namespaces that contain PVCs
#   - Preserves OpenBao KV paths (managementPolicies: Observe, Create)
#
# Force mode (-f):
#   - All safe-mode steps
#   - Tenant undeploy uses --purge (destructive tenant data removal)
#   - Also deletes data namespaces and bound PVs (full teardown)
#   - Removes Envoy Gateway, Kyverno (Gentian admission), and orphaned Gentian
#     OS cluster scaffold (gentianos.io CRDs/CRs, operator RBAC, catalogue)
#
# Usage:
#   ./uninstall.sh            # safe teardown
#   ./uninstall.sh -f         # full teardown (DESTROYS DATA)
#   ./uninstall.sh --keep-tenants   # leave tenant CRs/namespaces and Git manifests
#   ./uninstall.sh --cluster-infra  # also remove cert-manager/CNPG/reloader
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Shared helpers (composition glob, kernel Release discovery).
# shellcheck source=scripts/lib/load.sh
source "${SCRIPT_DIR}/scripts/lib/load.sh"

MODE="safe"
ENV="${ENV:-dev}"
UNINSTALL_CLUSTER_INFRA=0
UNINSTALL_KEEP_TENANTS=0
INSTALL_STATE_FILE="${INSTALL_STATE_FILE:-${SCRIPT_DIR}/.install-state.env}"
GENTIAN_MANAGED_CERT_MANAGER="${GENTIAN_MANAGED_CERT_MANAGER:-0}"

# Load install state to know if cert-manager is Gentian-managed.
# shellcheck source=/dev/null
if [[ -r "${INSTALL_STATE_FILE}" ]]; then
    source "${INSTALL_STATE_FILE}"
fi

# Deployments repo paths for tenant GitOps undeploy (install.env + plugin config).
INSTALL_CONFIG_FILE="${INSTALL_CONFIG_FILE:-${SCRIPT_DIR}/install.env}"
# shellcheck source=/dev/null
[[ -r "${INSTALL_CONFIG_FILE}" ]] && source "${INSTALL_CONFIG_FILE}"
# shellcheck source=/dev/null
[[ -r "${HOME}/.gentian/config" ]] && source "${HOME}/.gentian/config"
: "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"
: "${GENTIAN_DEPLOYMENTS_CLUSTER:=default-cluster}"
: "${GENTIAN_DEPLOYMENTS_STAGE:=${ENV}}"

SERVICES_NS="${SERVICES_NAMESPACE:-gentian-${ENV}}"
INFRA_NS="${INFRA_NAMESPACE:-gentian-infra-${ENV}}"
export SERVICES_NS INFRA_NS

while [[ $# -gt 0 ]]; do
    case "$1" in
        -f)                MODE="force" ;;
        --cluster-infra)   UNINSTALL_CLUSTER_INFRA=1 ;;
        --no-cluster-infra) UNINSTALL_CLUSTER_INFRA=0 ;;
        --keep-tenants)    UNINSTALL_KEEP_TENANTS=1 ;;
        -h|--help)
            echo "Usage: $0 [-f] [--keep-tenants] [--cluster-infra]"
            echo "  default          : safe uninstall (preserve PVC/PV data)"
            echo "  -f               : force uninstall (delete namespaces, bound PVs,"
            echo "                     Envoy Gateway, Kyverno, gentianos.io CRDs/RBAC)"
            echo "  --keep-tenants    : skip tenant undeploy (preserve tenant CRs, namespaces, Git manifests)"
            echo "  --cluster-infra   : also remove cert-manager/reloader/CNPG"
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
if [[ "${UNINSTALL_KEEP_TENANTS}" == "1" ]]; then
    echo -e "${CYAN}║     Tenants: KEEP (--keep-tenants)                       ║${NC}"
else
    echo -e "${CYAN}║     Tenants: undeploy all before kernel teardown         ║${NC}"
fi
echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""

if [[ "${MODE}" == "force" ]]; then
    if [[ "${UNINSTALL_KEEP_TENANTS}" == "1" ]]; then
        warn "FORCE mode: this will permanently delete Gentian OS kernel/data infrastructure."
        warn "Tenant workloads are preserved (--keep-tenants)."
    else
        warn "FORCE mode: this will permanently delete all Gentian OS data."
        warn "All tenants will be undeployed with --purge before kernel teardown."
    fi
    read -rp "  Type 'yes' to confirm: " confirm
    [[ "${confirm}" == "yes" ]] || { info "Aborted."; exit 0; }
fi

# Orphaned Kyverno webhook configs (cluster-scoped; survive a namespace
# deletion that bypasses Kyverno's own Helm uninstall hooks) fail closed and
# block ALL resource creation cluster-wide. Clean them up unconditionally,
# regardless of safe/force mode — this isn't data, just broken admission
# config, so there's no reason to gate it behind -f. See scripts/lib/common.sh.
cleanup_orphaned_kyverno_webhooks

_git_tenant_instances() {
    local tenants_root="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/tenants"
    local tenant_dir instance

    [[ -d "${tenants_root}" ]] || return 0

    # No <stage> segment here — a cluster has exactly one stage for its
    # whole lifetime (docs/deployment.md §1), so tenants/<instance>/tenant.yaml
    # is flat, not tenants/<instance>/<stage>/tenant.yaml.
    for tenant_dir in "${tenants_root}"/*; do
        [[ -d "${tenant_dir}" ]] || continue
        instance="$(basename "${tenant_dir}")"
        [[ -f "${tenant_dir}/tenant.yaml" ]] && echo "${instance}"
    done
}

_live_tenant_names() {
    kubectl get tenant --no-headers \
        -o custom-columns='NAME:.metadata.name' 2>/dev/null \
        | grep -v '^$' || true
}

# =============================================================================
# Step 0 — Undeploy all tenants (GitOps + in-cluster)
#
# Skipped when --keep-tenants is set.
#
# Two things must happen for a clean undeploy:
#  a) GitOps layer  — remove each tenant manifest from gentian-deployments
#     (deletionPolicy: Delete when force mode). `kubectl gentian tenants undeploy`
#     commits and pushes so ArgoCD does not re-create tenants on the next install.
#  b) In-cluster layer — delete each live Tenant CR and force-strip its finalizer
#     if the operator is absent or stuck.
#
# Git manifests are discovered from the deployments repo; live CRs from the cluster.
# Both are processed so uninstall is clean even when Git and cluster state diverge.
#
# Tenant CRs are managed by the gentian-os operator (gentian-system), which
# runs a cleanup finalizer (gentianos.io/tenant-cleanup).  The operator must
# be present and reachable for a clean deletion; if it is absent or stuck we
# force-strip the finalizer so uninstall can proceed.
#
# App CRs (apps.gentianos.io) live in each tenant namespace and are owned by
# the operator; delete them first so the operator's GC loop has less work.
# =============================================================================
if [[ "${UNINSTALL_KEEP_TENANTS}" == "1" ]]; then
    banner "Step 0 — Tenants (skipped)"
    info "Keeping tenant CRs, namespaces, and Git manifests (--keep-tenants)."
else
banner "Step 0 — Undeploy all tenants"

# The gentian-os operator registers a ValidatingWebhookConfiguration that
# intercepts PATCH on Tenant CRs.  If the operator is not running its
# webhook service is unavailable, causing kubectl patch to fail with
# "service not found".  Delete the webhook config first so we can
# force-strip finalizers without being blocked.
if kubectl get validatingwebhookconfiguration gentian-os-tenant-validator &>/dev/null; then
    info "Removing stale gentian-os-tenant-validator webhook (operator not running)..."
    kubectl delete validatingwebhookconfiguration gentian-os-tenant-validator \
        --ignore-not-found=true 2>/dev/null || true
fi

mapfile -t GIT_TENANT_INSTANCES < <(_git_tenant_instances | sort -u)
mapfile -t LIVE_TENANTS < <(_live_tenant_names | sort -u)

if [[ ${#GIT_TENANT_INSTANCES[@]} -eq 0 && ${#LIVE_TENANTS[@]} -eq 0 ]]; then
    info "No tenant manifests in Git and no live Tenant CRs; skipping."
else
    if [[ ${#GIT_TENANT_INSTANCES[@]} -gt 0 ]]; then
        info "Git tenant instance(s): ${GIT_TENANT_INSTANCES[*]}"
    fi
    if [[ ${#LIVE_TENANTS[@]} -gt 0 ]]; then
        info "Live Tenant CR(s): ${LIVE_TENANTS[*]}"
    fi

    # 0a. GitOps layer: remove each tenant manifest from gentian-deployments.
    if [[ ${#GIT_TENANT_INSTANCES[@]} -gt 0 ]]; then
        if command -v kubectl-gentian >/dev/null 2>&1; then
            _undeploy_flags=""
            [[ "${MODE}" == "force" ]] && _undeploy_flags="--purge"
            for instance in "${GIT_TENANT_INSTANCES[@]}"; do
                info "Removing ${instance} from deployments repo${_undeploy_flags:+ (purge mode)}..."
                # shellcheck disable=SC2086
                if kubectl gentian tenants undeploy "${instance}" ${_undeploy_flags} 2>&1; then
                    success "  Tenant instance ${instance} removed from deployments repo."
                else
                    warn "  kubectl gentian tenants undeploy failed for ${instance} — continuing with in-cluster cleanup."
                fi
            done
        else
            warn "kubectl-gentian plugin not found; skipping GitOps undeploy."
            warn "Remove clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/tenants/* manually to prevent ArgoCD from re-creating tenants on the next install."
        fi
    fi

    mapfile -t LIVE_TENANTS < <(_live_tenant_names | sort -u)
    if [[ ${#LIVE_TENANTS[@]} -eq 0 ]]; then
        info "No live Tenant CRs remain; GitOps undeploy complete."
    else
    for tenant_name in "${LIVE_TENANTS[@]}"; do
        local_deadline=$((SECONDS + 60))
        info "Deleting App CRs for tenant ${tenant_name}..."
        kubectl delete app --all \
            -l "gentianos.io/tenant=${tenant_name}" \
            --all-namespaces --ignore-not-found=true 2>/dev/null || \
        kubectl get app -A --no-headers 2>/dev/null \
            | awk -v t="${tenant_name}" '$0 ~ t {print $1, $2}' \
            | while read -r ns app_name; do
                kubectl delete app "${app_name}" -n "${ns}" --ignore-not-found=true 2>/dev/null || true
              done || true
    done

    # 2. Delete each Tenant CR and wait for the operator to remove the finalizer.
    #    If the operator is not running (timeout 60 s), force-strip the finalizer.
    for tenant_name in "${LIVE_TENANTS[@]}"; do
        if ! kubectl get tenant "${tenant_name}" &>/dev/null; then
            info "Tenant ${tenant_name} already gone; skipping."
            continue
        fi

        info "Deleting Tenant CR ${tenant_name}..."
        kubectl delete tenant "${tenant_name}" --ignore-not-found=true 2>/dev/null || true

        info "Waiting for Tenant ${tenant_name} finalizer to clear (max 60 s)..."
        local_deadline=$((SECONDS + 60))
        while kubectl get tenant "${tenant_name}" &>/dev/null; do
            if (( SECONDS > local_deadline )); then
                # Check whether the operator pod is running before deciding to
                # force-strip. If it is running, give it an extra 30 s to
                # complete an in-flight reconcile before we forcefully remove
                # the finalizer and risk orphaned resources.
                op_running=0
                kubectl get pods -n gentian-system -l "app.kubernetes.io/name=gentian-os" \
                    --field-selector=status.phase=Running --no-headers 2>/dev/null \
                    | grep -q . && op_running=1
                if [[ "${op_running}" == "1" ]]; then
                    warn "Operator is running but finalizer not cleared in 60 s — waiting 30 s more..."
                    sleep 30
                    kubectl get tenant "${tenant_name}" &>/dev/null || break
                fi
                warn "Tenant ${tenant_name} finalizer not cleared — force-stripping..."
                kubectl patch tenant "${tenant_name}" \
                    --type=json \
                    -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                    2>/dev/null || true
                break
            fi
            sleep 3
        done

        if kubectl get tenant "${tenant_name}" &>/dev/null; then
            warn "Tenant ${tenant_name} still present after finalizer strip — forcing deletion."
            kubectl delete tenant "${tenant_name}" \
                --grace-period=0 --force 2>/dev/null || true
        else
            success "Tenant ${tenant_name} removed."
        fi
    done

    success "All tenants undeployed."
    fi
fi
fi

# =============================================================================
# Step 1 — Remove Cluster XR claim and wait for Crossplane GC
# managementPolicies: [Observe, Create] on KV seeds means Crossplane will NOT
# delete the OpenBao KV paths when the XR is deleted. All other MRs
# (namespaces, policies, auth backend, etc.) ARE deleted by Crossplane GC.
# =============================================================================
banner "Step 1 — Remove Cluster XR (Crossplane GC)"

# InfraData XR owns shared postgres/mariadb Helm releases — remove before Cluster GC
# so provider-helm can uninstall chart resources cleanly.
if kubectl get infradata dev-infra-data -n crossplane-system >/dev/null 2>&1; then
    info "Deleting InfraData claim dev-infra-data..."
    kubectl delete infradata dev-infra-data -n crossplane-system --timeout=120s || true
else
    info "InfraData claim dev-infra-data not found; skipping."
fi

if kubectl get xinfradata dev-infra-data >/dev/null 2>&1; then
    info "Waiting for XInfraData dev-infra-data to be garbage-collected (max 5m)..."
    local_deadline=$((SECONDS + 300))
    while kubectl get xinfradata dev-infra-data >/dev/null 2>&1; do
        if (( SECONDS > local_deadline )); then
            warn "XInfraData dev-infra-data still present after 5m — forcing deletion."
            kubectl delete xinfradata dev-infra-data --grace-period=0 --force >/dev/null 2>&1 || true
            break
        fi
        sleep 5
    done
    success "XInfraData dev-infra-data removed."
else
    info "XInfraData dev-infra-data not found; skipping."
fi

# Suze XR (Keycloak + OpenFGA) — remove before Crossplane core so Helm releases GC cleanly.
if kubectl get suze dev-suze -n crossplane-system >/dev/null 2>&1; then
    info "Deleting Suze claim dev-suze..."
    kubectl delete suze dev-suze -n crossplane-system --timeout=120s || true
else
    info "Suze claim dev-suze not found; skipping."
fi

if kubectl get xsuze -o name 2>/dev/null | grep -q .; then
    info "Waiting for XSuze composite(s) to be garbage-collected (max 5m)..."
    local_deadline=$((SECONDS + 300))
    while kubectl get xsuze -o name 2>/dev/null | grep -q .; do
        if (( SECONDS > local_deadline )); then
            warn "XSuze still present after 5m — forcing finalizer removal."
            kubectl get xsuze -o name 2>/dev/null \
                | xargs -r -I{} kubectl patch {} \
                    --type=merge -p='{"metadata":{"finalizers":[]}}' \
                    2>/dev/null || true
            kubectl get xsuze -o name 2>/dev/null \
                | xargs -r kubectl delete --grace-period=0 --force \
                    2>/dev/null || true
            break
        fi
        sleep 5
    done
    success "XSuze composite(s) removed."
else
    info "XSuze composite not found; skipping."
fi

# Read the claim's real name rather than assuming the historical "dev-cluster"
# literal — clusters scaffolded after the rename are called <cluster>-<stage>,
# and deleting by the wrong name would silently leave the kernel provisioned.
CLUSTER_CLAIM_NAME="$(gentian_cluster_claim_name)"

if kubectl get cluster "${CLUSTER_CLAIM_NAME}" -n crossplane-system >/dev/null 2>&1; then
    info "Deleting Cluster claim ${CLUSTER_CLAIM_NAME}..."
    kubectl delete cluster "${CLUSTER_CLAIM_NAME}" -n crossplane-system --timeout=60s || true
else
    info "Cluster claim ${CLUSTER_CLAIM_NAME} not found; skipping."
fi

if kubectl get xcluster "${CLUSTER_CLAIM_NAME}" >/dev/null 2>&1; then
    info "Waiting for XCluster ${CLUSTER_CLAIM_NAME} to be garbage-collected (max 5m)..."
    local_deadline=$((SECONDS + 300))
    while kubectl get xcluster "${CLUSTER_CLAIM_NAME}" >/dev/null 2>&1; do
        if (( SECONDS > local_deadline )); then
            warn "XCluster ${CLUSTER_CLAIM_NAME} still present after 5m — forcing deletion."
            kubectl delete xcluster "${CLUSTER_CLAIM_NAME}" --grace-period=0 --force >/dev/null 2>&1 || true
            break
        fi
        sleep 5
    done
    success "XCluster ${CLUSTER_CLAIM_NAME} removed."
else
    info "XCluster ${CLUSTER_CLAIM_NAME} not found; skipping."
fi

# =============================================================================
# Step 1b — Remove kernel Pattern B Helm Releases and associated Secrets/ConfigMaps
# Must run before Crossplane providers are removed so Release GC can run.
# =============================================================================
banner "Step 1b — Remove kernel Pattern B Helm Releases"

# Pattern B Release CRs (kernel/services/*/manifests/<env>/release.yaml)
# Discovered from manifests (same source as update.sh --reconcile-releases), not a
# hardcoded app list. Tenant app Helm releases are removed with Tenant/App CRs.
delete_kernel_helm_releases "${ENV}"

for ns in "${SERVICES_NS}" "${INFRA_NS}"; do
    info "Removing kernel ConfigMaps / Secrets from ${ns}..."
    kubectl delete configmap \
        postgresql-base-values \
        postgresql-dev-values \
        mariadb-base-values \
        mariadb-dev-values \
        postfix-base-values \
        postfix-dev-values \
        dovecot-base-values \
        dovecot-dev-values \
        -n "${ns}" --ignore-not-found=true --timeout=30s 2>/dev/null || true
    kubectl delete externalsecret \
        portal-object-storage-credentials \
        postgresql-sensitive-values \
        mariadb-sensitive-values \
        postfix-sensitive-values \
        dovecot-sensitive-values \
        -n "${ns}" --ignore-not-found=true --timeout=30s 2>/dev/null || true
    # ESO-owned Secrets: delete only if ExternalSecrets are gone
    kubectl delete secret \
        portal-object-storage-credentials \
        postgresql-sensitive-values \
        mariadb-sensitive-values \
        postfix-sensitive-values \
        dovecot-sensitive-values \
        -n "${ns}" --ignore-not-found=true --timeout=30s 2>/dev/null || true
    kubectl delete secret registry-credentials \
        -n "${ns}" --ignore-not-found=true --timeout=30s 2>/dev/null || true
done

success "Kernel Pattern B resources removed."

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
            --timeout=10s 2>/dev/null || true
        kubectl delete "${mr}" --grace-period=0 --force \
            --ignore-not-found=true --timeout=10s 2>/dev/null || true
    done < <(kubectl get managed -o name 2>/dev/null)
    success "  Finalizers stripped; managed resources removed."
else
    success "  All managed resources drained."
fi

# =============================================================================
# Step 2 — Remove Crossplane compositions, XRDs, and ProviderConfigs
# =============================================================================
banner "Step 2 — Remove Crossplane XRDs, Compositions, ProviderConfigs"

delete_crossplane_compositions

if kubectl get crd compositeresourcedefinitions.apiextensions.crossplane.io >/dev/null 2>&1; then
    for file in \
        "${SCRIPT_DIR}/crossplane/xrds/app.yaml" \
        "${SCRIPT_DIR}/crossplane/xrds/tenant.yaml" \
        "${SCRIPT_DIR}/crossplane/xrds/cluster.yaml" \
        "${SCRIPT_DIR}/crossplane/xrds/suze.yaml"
    do
        if [[ -f "${file}" ]]; then
            kubectl delete -f "${file}" --ignore-not-found=true 2>/dev/null || true
            success "  Removed: $(basename "${file}")"
        fi
    done
    # Explicitly delete the CRDs that each XRD manages. If the Crossplane
    # package manager is not running (e.g. re-entrant uninstall), it won't GC
    # these, leaving them with an old ownerReference UID that blocks the next
    # install.sh run from establishing the same XRDs.
    #
    # Strip finalizers from all CR instances first; a CRD with finalizer-carrying
    # instances blocks kubectl delete indefinitely once the owning controller is
    # gone.  Also remove any stale webhook configurations that intercept PATCH
    # on these types before stripping (the webhook service may be absent).
    kubectl delete validatingwebhookconfiguration gentian-os-tenant-validator \
        --ignore-not-found=true 2>/dev/null || true

    for crd in \
        xapps.gentianos.io \
        apps.gentianos.io \
        xtenants.gentianos.io \
        tenants.gentianos.io \
        xclusters.gentianos.io \
        clusters.gentianos.io \
        xsuze.gentianos.io \
        suze.gentianos.io
    do
        # Strip finalizers from all CR instances before deleting the CRD.
        kubectl get "${crd}" -A -o name 2>/dev/null \
            | xargs -r -I{} kubectl patch {} \
                --type=merge -p='{"metadata":{"finalizers":[]}}' \
                2>/dev/null || true
        # Wait up to 30 s for the CRD to be fully removed.  If kubectl
        # times out, force-strip the CRD's own finalizers and verify.
        if kubectl delete crd "${crd}" --ignore-not-found=true --timeout=30s 2>/dev/null; then
            success "  CRD ${crd} removed."
        else
            warn "  CRD ${crd} timed out — force-stripping CRD finalizers..."
            kubectl patch crd "${crd}" \
                --type=merge -p='{"metadata":{"finalizers":[]}}' \
                2>/dev/null || true
            if ! kubectl get crd "${crd}" &>/dev/null; then
                success "  CRD ${crd} removed (after forced strip)."
            else
                warn "  CRD ${crd} could not be removed — check manually."
            fi
        fi
    done
    success "  XRD-owned CRDs removed."
else
    info "  XRD CRD absent; skipping XRD/CRD deletion."
fi

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

_delete_crossplane_crds

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
    gentian-os-kernel-identity-keycloak-bootstrap \
    gentian-os-kernel-mail-postfix \
    gentian-os-kernel-mail-dovecot \
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
        done || true
done

# Delete CRDs without waiting so we don't block on GC.
kubectl get crd -o name 2>/dev/null | grep "external-secrets.io" \
    | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true
success "ESO removed."

# =============================================================================
# Step 8 — Uninstall cert-manager (only if Gentian-managed)
# =============================================================================
cm_helm_present=0
if helm status cert-manager -n cert-manager >/dev/null 2>&1; then
    cm_helm_present=1
fi

# If install-state says Gentian-managed but there is no Helm release, treat it
# as stale state (e.g. cluster switched to distro addon-managed cert-manager).
if [[ "${GENTIAN_MANAGED_CERT_MANAGER}" == "1" && "${cm_helm_present}" != "1" ]]; then
    warn "GENTIAN_MANAGED_CERT_MANAGER=1 but no cert-manager Helm release found."
    warn "Treating cert-manager as externally managed for this uninstall run."
    GENTIAN_MANAGED_CERT_MANAGER="0"
fi

if [[ "${UNINSTALL_CLUSTER_INFRA}" == "1" || "${GENTIAN_MANAGED_CERT_MANAGER}" == "1" ]]; then
    banner "Step 8 — Uninstall cert-manager"

    if [[ "${cm_helm_present}" == "1" ]]; then
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
                done || true
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
# Step 9 — Remove additional cluster infra installed by install.sh
# =============================================================================
if [[ "${UNINSTALL_CLUSTER_INFRA}" == "1" ]]; then
    banner "Step 9 — Remove additional cluster infra"

    # Installed by install_argocd_image_updater() in scripts/lib/argocd.sh.
    if helm status argocd-image-updater -n argocd-image-updater >/dev/null 2>&1; then
        helm uninstall argocd-image-updater -n argocd-image-updater 2>/dev/null || true
        success "argocd-image-updater Helm release uninstalled."
    else
        info "argocd-image-updater Helm release not found; skipping uninstall."
    fi

    # Installed by bootstrap_argocd_apps() when cluster infra is enabled.
    # Remove CRDs as a best-effort cleanup in case the namespace teardown has
    # already removed the operator pod before finalizer processing completes.
    kubectl get crd -o name 2>/dev/null | grep "cnpg.io" \
        | xargs -r kubectl delete --ignore-not-found=true --wait=false 2>/dev/null || true

    success "Additional cluster infra cleanup queued."
fi

# =============================================================================
# Helper functions — namespace and volume cleanup
# =============================================================================

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

# =============================================================================
# Step 10 — Purge OpenBao data/secrets (force mode or cluster-infra)
# =============================================================================
if [[ "${MODE}" == "force" || "${UNINSTALL_CLUSTER_INFRA}" == "1" ]]; then
    banner "Step 10 — Purge OpenBao data/secrets"

    # Remove in-cluster OpenBao bootstrap/runtime secrets first.
    if kubectl get namespace openbao >/dev/null 2>&1; then
        kubectl delete secret --all -n openbao --ignore-not-found=true 2>/dev/null || true
        kubectl delete configmap --all -n openbao --ignore-not-found=true 2>/dev/null || true
        success "OpenBao namespace secrets/configmaps deleted."
    else
        info "OpenBao namespace not present; skipping in-namespace secret purge."
    fi

    # Force-remove OpenBao PVCs/PVs so no logical data survives via Retain PVs.
    _drain_pvcs "openbao"
    _delete_pvs_for_namespace "openbao"

    # If ESO CRDs are still present at this point, explicitly remove OpenBao stores.
    kubectl delete clustersecretstore openbao --ignore-not-found=true 2>/dev/null || true
    kubectl delete secretstore openbao -A --ignore-not-found=true 2>/dev/null || true

    success "OpenBao purge requested (KV backing storage, secrets, and stores)."
fi

# =============================================================================
# Step 10b — Remove AppProfile CRs (cluster-scoped catalog)
#
# AppProfiles are synced by the gentian-appprofiles ArgoCD Application from the
# gentian-apps repo.  They are cluster-scoped and survive namespace teardown
# because the appprofiles.gentianos.io CRD is not removed by uninstall.  Stale
# profiles (e.g. drifted valueMapping from a prior release) block a clean
# reinstall until ArgoCD can apply the current git state.
# =============================================================================
banner "Step 10b — Remove AppProfile CRs"

if kubectl get crd appprofiles.gentianos.io >/dev/null 2>&1; then
    _appprofile_count=$(kubectl get appprofiles.gentianos.io --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [[ "${_appprofile_count}" -gt 0 ]]; then
        info "Deleting ${_appprofile_count} AppProfile CR(s)..."
        while IFS= read -r _ap; do
            [[ -z "${_ap}" ]] && continue
            kubectl patch "${_ap}" \
                --type=merge -p='{"metadata":{"finalizers":[]}}' \
                2>/dev/null || true
            kubectl delete "${_ap}" --ignore-not-found=true --wait=false 2>/dev/null || true
        done < <(kubectl get appprofiles.gentianos.io -o name 2>/dev/null)
        success "AppProfile CRs removed."
    else
        info "No AppProfile CRs found; skipping."
    fi
else
    info "AppProfile CRD not found; skipping."
fi

# =============================================================================
# Step 11 — Remove kernel namespaces
# In safe mode, namespaces that contain PVCs are preserved.
# In force mode, all kernel namespaces are deleted.
# =============================================================================
banner "Step 11 — Remove kernel namespaces"

namespaces_to_remove=(openbao "${SERVICES_NS}" "${INFRA_NS}" gentian-system platform-kernel)
if [[ "${UNINSTALL_CLUSTER_INFRA}" == "1" ]]; then
    namespaces_to_remove+=(stakater-system cnpg-system argocd-image-updater)
fi

for ns in "${namespaces_to_remove[@]}"; do
    if ! kubectl get namespace "${ns}" >/dev/null 2>&1; then
        continue
    fi
    if [[ "${MODE}" == "force" ]]; then
        _drain_pvcs "${ns}"
        _delete_namespace "${ns}"
    elif _has_pvc "${ns}"; then
        warn "  Skipping namespace ${ns} (contains PVCs - use -f to delete)"
    else
        _delete_namespace "${ns}"
    fi
done

# =============================================================================
# Step 12 — Remove tenant resources (force mode, unless --keep-tenants)
# =============================================================================
if [[ "${MODE}" == "force" && "${UNINSTALL_KEEP_TENANTS}" -eq 0 ]]; then
    banner "Step 12 — Remove tenant resources"

    if kubectl get crd tenants.gentianos.io >/dev/null 2>&1; then
        info "Removing Tenant finalizers and deleting Tenant CRs..."
        while IFS= read -r tenant; do
            [[ -z "${tenant}" ]] && continue
            kubectl patch "${tenant}" \
                --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                2>/dev/null || true
            kubectl delete "${tenant}" --ignore-not-found=true --wait=false 2>/dev/null || true
        done < <(kubectl get tenants.gentianos.io -o name 2>/dev/null)
        success "Tenant CR deletion requested."
    else
        info "Tenant CRD not found; skipping Tenant CR deletion."
    fi

    mapfile -t tenant_namespaces < <(
        kubectl get namespaces -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
            | awk '/^tenant-/ {print $0}'
    )

    if [[ "${#tenant_namespaces[@]}" -eq 0 ]]; then
        info "No tenant-* namespaces found."
    else
        for ns in "${tenant_namespaces[@]}"; do
            _drain_pvcs "${ns}"
            _delete_namespace "${ns}"
        done
    fi
else
    if [[ "${UNINSTALL_KEEP_TENANTS}" == "1" ]]; then
        info "Skipping tenant resource removal (--keep-tenants)."
    else
        info "Skipping tenant resource removal (only enabled with -f)."
    fi
fi

# =============================================================================
# Step 13 — Force-mode platform scaffold cleanup
#
# Orphaned cluster-scoped resources (Envoy Gateway, Kyverno, gentianos.io CRDs,
# operator RBAC, catalogue CRs) survive namespace teardown.  Remove them in force
# mode so the next install starts from a clean API surface.
# =============================================================================
if [[ "${MODE}" == "force" ]]; then
    banner "Step 13 — Force-mode platform scaffold cleanup"
    _delete_envoy_gateway_scaffold
    _delete_kyverno_scaffold
    _delete_gentianos_api_scaffold
else
    info "Skipping platform scaffold cleanup (only enabled with -f)."
    info "  Re-run with -f to also remove Envoy Gateway, Kyverno, and gentianos.io CRDs/RBAC."
fi

# =============================================================================
# Remove host CLI tools (kubectl-gentian plugin + gtnctl symlink)
# =============================================================================
banner "Remove host CLI tools"

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

_remove_host_cli /usr/local/bin/gtnctl
_remove_host_cli /usr/local/bin/kubectl-gentian

# =============================================================================
# Done
# =============================================================================
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║     Gentian OS — Uninstall complete                      ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""
if [[ "${MODE}" == "force" || "${UNINSTALL_CLUSTER_INFRA}" == "1" ]]; then
    echo "  OpenBao KV/secrets purge was requested (force/cluster-infra mode)."
    echo "  Verify with: kubectl get ns openbao && kubectl get pv | grep openbao"
else
    echo "  OpenBao KV data is PRESERVED (managementPolicies: Observe/Create"
    echo "  prevents Crossplane from deleting KV paths on XR deletion)."
fi
echo ""
if [[ "${UNINSTALL_KEEP_TENANTS}" == "1" ]]; then
    echo "  Tenant CRs, namespaces, and Git manifests were preserved (--keep-tenants)."
    echo ""
fi
if [[ "${MODE}" == "safe" ]]; then
    echo "  PVC/PV data is preserved (safe mode)."
    echo "  Re-run with -f to also remove namespaces, bound PVs, Envoy Gateway,"
    echo "  Kyverno, and orphaned gentianos.io CRDs/RBAC."
fi

# Clear the persisted run-start epoch so the next install starts with a fresh
# stale-data cutoff (otherwise PVCs created during the upcoming install could
# be misclassified as stale relative to a previous run's epoch).
if [[ -f "${INSTALL_STATE_FILE}" ]]; then
    sed -i '/^export INSTALL_START_EPOCH=/d' "${INSTALL_STATE_FILE}" 2>/dev/null || true
fi

echo ""
echo "  To re-install: ./install.sh"
echo ""
