#!/usr/bin/env bash
# P4 E2E — Cutover verification for an existing tenant.
#
# Validates that a live tenant (default: demo) is fully converged on the
# Crossplane graph: manifest bridge, XTenant Ready, no duplicate App claims,
# and operator CrossplaneReady status aggregation.
#
# Usage:
#   make e2e-p4
#   TENANT=demo make e2e-p4
#
# Prerequisites:
#   - install.sh completed and tenant deployed (e.g. kubectl gentian tenants deploy demo)
#   - tenant manifest bridge active on the cluster
#
# Optional smoke (set RUN_SMOKE=1):
#   Requires tenant apps to expose HTTP routes; checks one app hostname returns 2xx/3xx.
set -euo pipefail

TENANT="${TENANT:-demo}"
KERNEL_NS="${KERNEL_NS:-platform-kernel}"
TIMEOUT_XTENANT="${TIMEOUT_XTENANT:-2m}"
RUN_SMOKE="${RUN_SMOKE:-0}"

info()  { echo "[INFO]  $*"; }
pass()  { echo "[PASS]  $*"; }
warn()  { echo "[WARN]  $*"; }
fail()  { echo "[FAIL]  $*"; exit 1; }

# ── Pre-checks ──────────────────────────────────────────────────────────────

info "Checking prerequisites for tenant ${TENANT}..."
command -v kubectl >/dev/null 2>&1 || fail "kubectl not found"

kubectl cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || fail "No reachable cluster — set KUBECONFIG"

kubectl get tenant "${TENANT}" >/dev/null 2>&1 \
  || fail "Tenant ${TENANT} not found — deploy it first"

kubectl get xtenant "${TENANT}" >/dev/null 2>&1 \
  || fail "XTenant ${TENANT} not found — operator should create it from Tenant CR"

# ── Manifest bridge ─────────────────────────────────────────────────────────

PROV_CM="tenant-${TENANT}-provisioning-jobs"
info "Checking provisioning ConfigMap ${PROV_CM}..."
kubectl get configmap "${PROV_CM}" -n "${KERNEL_NS}" >/dev/null 2>&1 \
  || fail "ConfigMap ${PROV_CM} missing — operator manifest bridge not active"

jobs_json=$(kubectl get configmap "${PROV_CM}" -n "${KERNEL_NS}" -o jsonpath='{.data.jobs\.json}')
objects_json=$(kubectl get configmap "${PROV_CM}" -n "${KERNEL_NS}" -o jsonpath='{.data.objects\.json}')
[[ -n "${jobs_json}" && "${jobs_json}" != "[]" ]] || fail "jobs.json empty in ${PROV_CM}"
[[ -n "${objects_json}" ]] || fail "objects.json missing in ${PROV_CM}"
pass "Manifest bridge ConfigMap has jobs.json and objects.json"

# ── XTenant + operator status ───────────────────────────────────────────────

info "Checking XTenant ${TENANT} Ready (timeout: ${TIMEOUT_XTENANT})..."
if ! kubectl wait "xtenant/${TENANT}" --for=condition=Ready --timeout="${TIMEOUT_XTENANT}"; then
    fail "XTenant ${TENANT} not Ready — investigate before cutover:
           kubectl describe xtenant ${TENANT}
           kubectl get managed -l crossplane.io/composite=${TENANT}"
fi
pass "XTenant ${TENANT} is Ready"

cp_ready=$(kubectl get tenant "${TENANT}" \
    -o jsonpath='{.status.conditions[?(@.type=="CrossplaneReady")].status}')
[[ "${cp_ready}" == "True" ]] \
  || fail "Tenant CrossplaneReady=${cp_ready:-<unset>} — status aggregation not converged"
pass "Tenant CrossplaneReady=True"

tenant_phase=$(kubectl get tenant "${TENANT}" -o jsonpath='{.status.phase}')
info "Tenant phase: ${tenant_phase:-<unset>}"

# ── Single owner checks ───────────────────────────────────────────────────────

NS=$(kubectl get tenant "${TENANT}" -o jsonpath='{.status.namespace}')
[[ -z "${NS}" ]] && NS="tenant-${TENANT}"

info "Checking App claims in ${NS}..."
app_spec_count=$(kubectl get tenant "${TENANT}" -o json | \
    python3 -c "import json,sys; t=json.load(sys.stdin); print(len(t.get('spec',{}).get('apps',[])))" 2>/dev/null || echo 0)
app_claim_count=$(kubectl get app -n "${NS}" --no-headers 2>/dev/null | wc -l | tr -d ' ')

if [[ "${app_spec_count}" -eq 0 ]]; then
    info "Tenant has no apps in spec — skipping App claim count check"
elif [[ "${app_claim_count}" -eq "${app_spec_count}" ]]; then
    pass "Exactly ${app_claim_count} App claim(s) (matches spec.apps)"
else
    fail "App claim count mismatch: spec=${app_spec_count} cluster=${app_claim_count} — possible duplicate ownership"
fi

crossplane_apps=$(kubectl get app -n "${NS}" -l gentianos.io/managed-by=crossplane --no-headers 2>/dev/null | wc -l | tr -d ' ')
if [[ "${app_spec_count}" -gt 0 && "${crossplane_apps}" -lt "${app_spec_count}" ]]; then
    warn "Only ${crossplane_apps}/${app_spec_count} App claims labelled managed-by=crossplane"
else
    pass "App claims owned by Crossplane (${crossplane_apps}/${app_spec_count})"
fi

mr_count=$(kubectl get managed -l "crossplane.io/composite=${TENANT}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
[[ "${mr_count}" -gt 0 ]] || fail "No managed resources for composite=${TENANT}"
pass "${mr_count} managed resource(s) in Crossplane graph"

job_mr_count=$(kubectl get object.kubernetes.crossplane.io -o name 2>/dev/null \
    | grep -c "${TENANT}-job-" || true)
[[ "${job_mr_count}" -gt 0 ]] || fail "No Crossplane job Object MRs for ${TENANT}"
pass "${job_mr_count} Crossplane job Object MR(s)"

obj_mr_count=$(kubectl get object.kubernetes.crossplane.io -o name 2>/dev/null \
    | grep -c "${TENANT}-obj-" || true)
if [[ "${obj_mr_count}" -gt 0 ]]; then
    pass "${obj_mr_count} Crossplane object Object MR(s) (data plane / edge)"
else
    warn "No ${TENANT}-obj-* Object MRs — expected when tenant has DB/cache/gateway apps"
fi

# ── Optional HTTP smoke ─────────────────────────────────────────────────────

if [[ "${RUN_SMOKE}" == "1" ]]; then
    info "Running HTTP smoke checks (RUN_SMOKE=1)..."
    kernel_domain=$(kubectl get deployment -A -l app.kubernetes.io/name=gentian-os \
        -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="KERNEL_DOMAIN")].value}' 2>/dev/null || true)
    if [[ -z "${kernel_domain}" ]]; then
        warn "KERNEL_DOMAIN not found on operator — skipping HTTP smoke"
    else
        # First app subdomain from spec (element → chat.{tenant}.{kernel} in multi mode).
        first_profile=$(kubectl get tenant "${TENANT}" -o jsonpath='{.spec.apps[0].profile}')
        if [[ -n "${first_profile}" ]]; then
            host="chat.${TENANT}.${kernel_domain}"
            info "  curl -sfI https://${host}/ (best-effort)"
            if curl -sfI --max-time 15 "https://${host}/" >/dev/null 2>&1; then
                pass "  HTTPS ${host} reachable"
            else
                warn "  HTTPS ${host} not reachable — may be expected on isolated dev clusters"
            fi
        fi
    fi
fi

pass "All P4 cutover checks passed"
echo ""
echo "P4 E2E complete. Tenant ${TENANT} is converged on the Crossplane graph."
echo "To validate Crossplane-only mode: set tenantProvisioning.crossplaneOnly=true"
echo "(TENANT_CROSSPLANE_ONLY) on the operator and re-run this script."
