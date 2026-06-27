#!/usr/bin/env bash
# P2 E2E — Pattern B kernel charts via provider-helm on the dev cluster.
#
# Verifies kernel-facing Helm Release MRs under kernel/services/*/manifests/ are
# Synced + Ready, values do not embed plaintext secrets, and core kernel pods run.
#
# Usage:  make e2e-p2
#
# Prerequisites:
#   - install.sh completed (Pattern B Releases applied)
#   - KUBECONFIG pointing at the dev cluster
set -euo pipefail

SERVICES_NS="${SERVICES_NAMESPACE:-gentian-dev}"
TIMEOUT_RELEASE="${TIMEOUT_RELEASE:-15m}"

info()  { echo "[INFO]  $*"; }
pass()  { echo "[PASS]  $*"; }
warn()  { echo "[WARN]  $*"; }
fail()  { echo "[FAIL]  $*"; exit 1; }

info "Checking prerequisites..."
command -v kubectl >/dev/null 2>&1 || fail "kubectl not found"
command -v jq >/dev/null 2>&1 || fail "jq not found (run: make install-tools)"
kubectl cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || fail "No reachable cluster — set KUBECONFIG"

# ── Kernel Pattern B Release MRs ─────────────────────────────────────────────

info "Collecting kernel Helm Release MRs (namespace ${SERVICES_NS}, excluding tenant App Releases)..."
mapfile -t releases < <(kubectl get release.helm.crossplane.io -n "${SERVICES_NS}" \
  -o jsonpath='{range .items[?(@.metadata.labels.crossplane\.io/composite=="")]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
# Fallback for clusters without empty-string selector support
if [[ ${#releases[@]} -eq 0 ]]; then
  mapfile -t releases < <(kubectl get release.helm.crossplane.io -n "${SERVICES_NS}" -o json \
    | jq -r '.items[] | select(.metadata.labels["crossplane.io/composite"] == null) | .metadata.name' 2>/dev/null || true)
fi

if [[ ${#releases[@]} -eq 0 ]]; then
  warn "No standalone kernel Helm Releases in ${SERVICES_NS} — InfraData XR may own postgres/mariadb"
else
  pass "Found ${#releases[@]} standalone Helm Release(s) in ${SERVICES_NS}"
fi

failed=0

# InfraData XR — shared postgres/mariadb (Step 2 kernel rebuild)
info "Checking InfraData shared database Releases..."
for rel in dev-infra-data-postgresql dev-infra-data-mariadb; do
  if kubectl get release.helm.crossplane.io/"${rel}" >/dev/null 2>&1; then
    if kubectl wait "release.helm.crossplane.io/${rel}" \
        --for=condition=Synced --timeout="${TIMEOUT_RELEASE}" 2>/dev/null \
    && kubectl wait "release.helm.crossplane.io/${rel}" \
        --for=condition=Ready --timeout="${TIMEOUT_RELEASE}" 2>/dev/null; then
      pass "InfraData Release ${rel} Synced + Ready"
    else
      warn "InfraData Release ${rel} not Synced/Ready"
      failed=1
    fi
  else
    if [[ "${rel}" == *postgresql* ]]; then
      legacy="opendesk-postgresql-dev"
    else
      legacy="opendesk-mariadb-dev"
    fi
    if kubectl get release.helm.crossplane.io/"${legacy}" >/dev/null 2>&1; then
      pass "Legacy infra Release ${legacy} present (pre–Step 2 migration)"
    else
      warn "Neither InfraData nor legacy Release found for ${rel}"
    fi
  fi
done

info "Waiting for kernel Releases Synced + Ready (timeout: ${TIMEOUT_RELEASE})..."
for rel in "${releases[@]}"; do
  if ! kubectl wait "release.helm.crossplane.io/${rel}" -n "${SERVICES_NS}" \
      --for=condition=Synced --timeout="${TIMEOUT_RELEASE}" 2>/dev/null; then
    warn "Release ${rel} not Synced"
    failed=1
    continue
  fi
  if ! kubectl wait "release.helm.crossplane.io/${rel}" -n "${SERVICES_NS}" \
      --for=condition=Ready --timeout="${TIMEOUT_RELEASE}" 2>/dev/null; then
    warn "Release ${rel} not Ready"
    failed=1
    continue
  fi
  pass "Release ${rel} Synced + Ready"
done
[[ $failed -eq 0 ]] || fail "One or more kernel Releases not Synced/Ready — kubectl describe release.helm.crossplane.io -n ${SERVICES_NS}"

# ── Secret hygiene in Release values ─────────────────────────────────────────

info "Checking Release specs for inline secret values..."
# Pattern B charts should use valuesFrom / existingSecret — not plaintext passwords.
inline_secret_hits=0
while IFS= read -r rel; do
  [[ -z "$rel" ]] && continue
  yaml=$(kubectl get release.helm.crossplane.io/"${rel}" -n "${SERVICES_NS}" -o yaml 2>/dev/null || true)
  if echo "$yaml" | grep -Eiq 'password:[[:space:]]*[^$"{]|clientSecret:[[:space:]]*[^$"{]|bindCredential:[[:space:]]*[^$"{]'; then
    warn "Release ${rel} may contain inline secret in spec.values (review manually)"
    inline_secret_hits=$((inline_secret_hits + 1))
  fi
done < <(printf '%s\n' "${releases[@]}")
if [[ $inline_secret_hits -gt 0 ]]; then
  warn "${inline_secret_hits} Release(s) flagged for possible inline secrets — prefer valuesFrom / existingSecret"
else
  pass "No obvious inline secrets in Release spec.values"
fi

# ── Core kernel pod health ───────────────────────────────────────────────────

info "Checking core kernel workloads in ${SERVICES_NS}..."
check_deployment_ready() {
  local deploy="$1"
  if ! kubectl get deployment "$deploy" -n "${SERVICES_NS}" >/dev/null 2>&1; then
    warn "Deployment ${deploy} not found (skipped)"
    return 0
  fi
  kubectl wait "deployment/${deploy}" -n "${SERVICES_NS}" --for=condition=Available \
    --timeout="${TIMEOUT_RELEASE}" >/dev/null 2>&1 \
    || fail "Deployment ${deploy} not Available"
  pass "Deployment ${deploy} Available"
}

# Nubus (Keycloak/LDAP stack), mail, Nextcloud — representative Pattern B services
check_deployment_ready "nubus-dev-portal-server"
check_deployment_ready "postfix-dev"
check_deployment_ready "dovecot-dev"
check_deployment_ready "nextcloud-dev-aio"

# LDAP server is a StatefulSet
if kubectl get statefulset nubus-dev-ldap-server-primary -n "${SERVICES_NS}" >/dev/null 2>&1; then
  ready=$(kubectl get statefulset nubus-dev-ldap-server-primary -n "${SERVICES_NS}" \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
  desired=$(kubectl get statefulset nubus-dev-ldap-server-primary -n "${SERVICES_NS}" \
    -o jsonpath='{.status.replicas}' 2>/dev/null || echo "0")
  if [[ "${ready:-0}" -ge 1 && "${ready}" == "${desired}" ]]; then
    pass "StatefulSet nubus-dev-ldap-server-primary ready (${ready}/${desired})"
  else
    fail "StatefulSet nubus-dev-ldap-server-primary not ready (${ready}/${desired})"
  fi
else
  warn "StatefulSet nubus-dev-ldap-server-primary not found (skipped)"
fi

if kubectl get statefulset nubus-dev-keycloak -n "${SERVICES_NS}" >/dev/null 2>&1; then
  ready=$(kubectl get statefulset nubus-dev-keycloak -n "${SERVICES_NS}" \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
  desired=$(kubectl get statefulset nubus-dev-keycloak -n "${SERVICES_NS}" \
    -o jsonpath='{.status.replicas}' 2>/dev/null || echo "0")
  if [[ "${ready:-0}" -ge 1 && "${ready}" == "${desired}" ]]; then
    pass "StatefulSet nubus-dev-keycloak ready (${ready}/${desired})"
  else
    fail "StatefulSet nubus-dev-keycloak not ready (${ready}/${desired})"
  fi
else
  warn "StatefulSet nubus-dev-keycloak not found (skipped)"
fi

pass "P2 Pattern B kernel chart verification complete"
