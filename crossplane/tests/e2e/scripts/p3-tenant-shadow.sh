#!/usr/bin/env bash
# P3 E2E — Shadow tenant provisioned via Crossplane graph.
#
# Applies a throwaway Tenant CR and verifies the operator orchestrates (seed +
# manifest bridge + XTenant patch) while Crossplane owns resource creation.
#
# Usage:
#   make e2e-p3
#   SHADOW_TENANT=shadow-e2e make e2e-p3
#
# Prerequisites:
#   - install.sh completed (Crossplane, XRDs, Compositions, operator, AppProfiles)
#   - KUBECONFIG pointing at the dev/test cluster
#   - OpenBao running; operator Seeder configured
#
# Rollback:
#   kubectl delete tenant "${SHADOW_TENANT}" --wait=false
set -euo pipefail

SHADOW_TENANT="${SHADOW_TENANT:-shadow-e2e}"
TIMEOUT_XTENANT="${TIMEOUT_XTENANT:-25m}"
TIMEOUT_CM="${TIMEOUT_CM:-5m}"
KERNEL_NS="${KERNEL_NS:-platform-kernel}"

info()  { echo "[INFO]  $*"; }
pass()  { echo "[PASS]  $*"; }
warn()  { echo "[WARN]  $*"; }
fail()  { echo "[FAIL]  $*"; exit 1; }

cleanup() {
    if [[ "${SKIP_CLEANUP:-0}" == "1" ]]; then
        warn "SKIP_CLEANUP=1 — leaving tenant ${SHADOW_TENANT} on cluster"
        return 0
    fi
    info "Cleaning up shadow tenant ${SHADOW_TENANT}..."
    kubectl delete tenant "${SHADOW_TENANT}" --ignore-not-found=true --wait=false 2>/dev/null || true
    local deadline=$((SECONDS + 300))
    while kubectl get namespace "tenant-${SHADOW_TENANT}" >/dev/null 2>&1; do
        if (( SECONDS > deadline )); then
            warn "tenant-${SHADOW_TENANT} namespace still present after 5m"
            break
        fi
        sleep 5
    done
    pass "Shadow tenant cleanup initiated"
}
trap cleanup EXIT

# ── Pre-checks ──────────────────────────────────────────────────────────────

info "Checking prerequisites..."
command -v kubectl >/dev/null 2>&1 || fail "kubectl not found"

kubectl cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || fail "No reachable cluster — set KUBECONFIG"

info "Cluster: $(kubectl config current-context)"

kubectl get deployment crossplane -n crossplane-system >/dev/null 2>&1 \
  || fail "Crossplane not found — run install.sh first"

kubectl wait xrd xtenants.gentianos.io --for=condition=Established --timeout=2m \
  || fail "XTenant XRD not Established"

kubectl get composition tenant-default >/dev/null 2>&1 \
  || fail "Composition tenant-default missing — run install.sh or update.sh --crossplane"

kubectl get deployment -A -l app.kubernetes.io/name=gentian-os --no-headers 2>/dev/null | grep -q . \
  || fail "gentian-os operator Deployment not found — run install.sh Step 15"

if kubectl get tenant "${SHADOW_TENANT}" >/dev/null 2>&1; then
    fail "Tenant ${SHADOW_TENANT} already exists — delete it or set SHADOW_TENANT to another name"
fi

# ── Apply shadow Tenant ───────────────────────────────────────────────────────

info "Applying shadow Tenant ${SHADOW_TENANT} (identity shell, no apps)..."
kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: ${SHADOW_TENANT}
spec:
  adminEmail: ${SHADOW_TENANT}@gentian.org
  displayName: Shadow E2E
  deletionPolicy: Delete
  isolation:
    databasePrefix: ${SHADOW_TENANT}_
    keycloakRealm: ${SHADOW_TENANT}
    ldapOU: ou=${SHADOW_TENANT}
    s3Prefix: ${SHADOW_TENANT}-
  apps: []
EOF

# ── Wait for manifest bridge ConfigMap ───────────────────────────────────────

PROV_CM="tenant-${SHADOW_TENANT}-provisioning-jobs"
info "Waiting for provisioning ConfigMap ${PROV_CM} (timeout: ${TIMEOUT_CM})..."
if ! kubectl wait configmap/"${PROV_CM}" -n "${KERNEL_NS}" --for=jsonpath='{.metadata.name}'="${PROV_CM}" --timeout="${TIMEOUT_CM}" 2>/dev/null; then
    # kubectl wait on create may be unavailable on older kubectl; fall back to poll.
    cm_deadline=$((SECONDS + 300))
    while ! kubectl get configmap "${PROV_CM}" -n "${KERNEL_NS}" >/dev/null 2>&1; do
        if (( SECONDS > cm_deadline )); then
            fail "ConfigMap ${PROV_CM} not created within ${TIMEOUT_CM}"
        fi
        sleep 3
    done
fi
pass "Provisioning ConfigMap ${PROV_CM} exists"

jobs_key=$(kubectl get configmap "${PROV_CM}" -n "${KERNEL_NS}" -o jsonpath='{.data.jobs\.json}' 2>/dev/null || true)
objects_key=$(kubectl get configmap "${PROV_CM}" -n "${KERNEL_NS}" -o jsonpath='{.data.objects\.json}' 2>/dev/null || true)
[[ -n "${jobs_key}" && "${jobs_key}" != "[]" ]] \
  || fail "jobs.json missing or empty in ${PROV_CM}"
[[ -n "${objects_key}" ]] \
  || fail "objects.json missing in ${PROV_CM} (manifest bridge)"
pass "ConfigMap contains jobs.json and objects.json"

# ── Wait for XTenant Ready ───────────────────────────────────────────────────

info "Waiting for XTenant ${SHADOW_TENANT} Ready (timeout: ${TIMEOUT_XTENANT})..."
if ! kubectl wait "xtenant/${SHADOW_TENANT}" --for=condition=Ready --timeout="${TIMEOUT_XTENANT}"; then
    fail "XTenant ${SHADOW_TENANT} did not become Ready. Check:
           kubectl describe xtenant ${SHADOW_TENANT}
           kubectl get managed -l crossplane.io/composite=${SHADOW_TENANT}"
fi
pass "XTenant ${SHADOW_TENANT} is Ready"

# ── Spot checks ─────────────────────────────────────────────────────────────

info "Spot-checking Crossplane-owned resources..."
ERRORS=0

check_exists() {
    local desc="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        pass "  ${desc}"
    else
        warn "  ${desc} — NOT FOUND"
        ERRORS=$((ERRORS + 1))
    fi
}

NS="tenant-${SHADOW_TENANT}"
check_exists "namespace ${NS}" kubectl get namespace "${NS}"
check_exists "Object MR ${SHADOW_TENANT}-namespace" \
    kubectl get object.kubernetes.crossplane.io "${SHADOW_TENANT}-namespace"
check_exists "Vault policy MR ${SHADOW_TENANT}-tenant-policy" \
    kubectl get policy.vault.upbound.io "${SHADOW_TENANT}-tenant-policy"

# At least one Crossplane job Object MR (realm provisioning).
job_mr_count=$(kubectl get object.kubernetes.crossplane.io -o name 2>/dev/null \
    | grep -c "${SHADOW_TENANT}-job-" || true)
if [[ "${job_mr_count}" -gt 0 ]]; then
    pass "  ${job_mr_count} Crossplane job Object MR(s) for ${SHADOW_TENANT}"
else
    warn "  no Crossplane job Object MRs found for ${SHADOW_TENANT}"
    ERRORS=$((ERRORS + 1))
fi

# Tenant.status should reflect Crossplane readiness.
cp_ready=$(kubectl get tenant "${SHADOW_TENANT}" \
    -o jsonpath='{.status.conditions[?(@.type=="CrossplaneReady")].status}' 2>/dev/null || true)
if [[ "${cp_ready}" == "True" ]]; then
    pass "  Tenant CrossplaneReady=True"
else
    warn "  Tenant CrossplaneReady not True (got: ${cp_ready:-<unset>})"
    ERRORS=$((ERRORS + 1))
fi

# Managed resource graph should be non-empty.
mr_count=$(kubectl get managed -l "crossplane.io/composite=${SHADOW_TENANT}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
if [[ "${mr_count}" -gt 0 ]]; then
    pass "  ${mr_count} managed resource(s) labelled composite=${SHADOW_TENANT}"
else
    warn "  no managed resources for composite=${SHADOW_TENANT}"
    ERRORS=$((ERRORS + 1))
fi

if [[ "${ERRORS}" -gt 0 ]]; then
    fail "${ERRORS} spot-check(s) failed"
fi

pass "All P3 spot-checks passed"
echo ""
echo "P3 E2E complete. Shadow tenant ${SHADOW_TENANT} converged via Crossplane graph."
SKIP_CLEANUP=0
