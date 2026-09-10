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
releases=()
while IFS= read -r _rel; do
  [[ -n "${_rel}" ]] && releases+=("${_rel}")
done < <(kubectl get release.helm.crossplane.io -n "${SERVICES_NS}" \
  -o jsonpath='{range .items[?(@.metadata.labels.crossplane\.io/composite=="")]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
# Fallback for clusters without empty-string selector support
if [[ ${#releases[@]} -eq 0 ]]; then
  releases=()
  while IFS= read -r _rel; do
    [[ -n "${_rel}" ]] && releases+=("${_rel}")
  done < <(kubectl get release.helm.crossplane.io -n "${SERVICES_NS}" -o json \
    | jq -r '.items[] | select(.metadata.labels["crossplane.io/composite"] == null) | .metadata.name' 2>/dev/null || true)
fi

if [[ ${#releases[@]} -eq 0 ]]; then
  warn "No standalone kernel Helm Releases in ${SERVICES_NS} — InfraData XR may own postgres/mariadb"
else
  pass "Found ${#releases[@]} standalone Helm Release(s) in ${SERVICES_NS}"
fi

failed=0

# InfraData XR — shared postgres/mariadb/redis/minio
info "Checking InfraData shared infra Releases..."
for rel in dev-infra-data-postgresql dev-infra-data-mariadb dev-infra-data-redis dev-infra-data-minio; do
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
    warn "InfraData Release ${rel} not found"
    failed=1
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

info "Checking core kernel workloads..."
check_statefulset_ready() {
  local sts="$1"
  local ns="$2"
  if ! kubectl get statefulset "$sts" -n "$ns" >/dev/null 2>&1; then
    return 1
  fi
  local ready desired
  ready=$(kubectl get statefulset "$sts" -n "$ns" \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
  desired=$(kubectl get statefulset "$sts" -n "$ns" \
    -o jsonpath='{.status.replicas}' 2>/dev/null || echo "0")
  if [[ "${ready:-0}" -ge 1 && "${ready}" == "${desired}" ]]; then
    pass "StatefulSet ${sts} ready (${ready}/${desired}) in ${ns}"
    return 0
  fi
  fail "StatefulSet ${sts} not ready (${ready}/${desired}) in ${ns}"
}

keycloak_found=0
for ns in platform-kernel "${SERVICES_NS}"; do
  for sts in gentian-idp-keycloak-keycloakx gentian-idp-keycloak; do
    if kubectl get statefulset "$sts" -n "$ns" >/dev/null 2>&1; then
      check_statefulset_ready "$sts" "$ns"
      keycloak_found=1
      break 2
    fi
  done
done
if [[ $keycloak_found -eq 0 ]]; then
  warn "Suze Keycloak StatefulSet not found in platform-kernel or ${SERVICES_NS} (skipped)"
fi

pass "P2 Pattern B kernel chart verification complete"
