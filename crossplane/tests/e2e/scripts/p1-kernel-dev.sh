#!/usr/bin/env bash
# P1 E2E — Cluster XRD + Composition: kernel structural provisioning on dev.
#
# What this test does:
#   1. Install providers (provider-kubernetes, provider-vault, function-go-templating)
#   2. Wait for providers to become Healthy
#   3. Apply ProviderConfigs
#   4. Apply XRD (XCluster / Cluster) and Composition (cluster-default)
#   5. Apply the dev-cluster Cluster claim
#   6. Wait for the XCluster composite to be Ready
#   7. Spot-check key managed resources are present
#
# Usage:  make e2e-p1
# Rollback: kubectl delete cluster dev-cluster -n crossplane-system
#           kubectl delete xcluster dev-cluster
#
# Prerequisites:
#   - Phase 0 complete (Crossplane core running in crossplane-system)
#   - KUBECONFIG pointing at the dev cluster
#   - OpenBao running at http://openbao.openbao.svc.cluster.local:8200
#   - K8s Secret 'gentian-os-master-password' in namespace crossplane-system
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

TIMEOUT_PROVIDERS=10m
TIMEOUT_XR=15m

info()  { echo "[INFO]  $*"; }
pass()  { echo "[PASS]  $*"; }
warn()  { echo "[WARN]  $*"; }
fail()  { echo "[FAIL]  $*"; exit 1; }

# ── Pre-checks ──────────────────────────────────────────────────────────────

info "Checking prerequisites..."
command -v kubectl >/dev/null 2>&1   || fail "kubectl not found"
command -v crossplane >/dev/null 2>&1 || fail "crossplane CLI not found (run: make install-tools)"

kubectl cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || fail "No reachable cluster — point KUBECONFIG at your dev cluster."

info "Cluster: $(kubectl config current-context)"

# Crossplane must be running (Phase 0)
kubectl get deployment crossplane -n crossplane-system >/dev/null 2>&1 \
  || fail "Crossplane core not found in crossplane-system — run: make e2e-p0 first"

# ── Step 1: Install providers and function ──────────────────────────────────

info "Applying providers (function-go-templating, provider-kubernetes, provider-vault)..."
kubectl apply -f "${REPO_ROOT}/providers/providers.yaml"

info "Waiting for providers to become Healthy (timeout: ${TIMEOUT_PROVIDERS})..."
for provider in function-go-templating function-extra-resources function-auto-ready function-sequencer provider-kubernetes provider-vault; do
  info "  Waiting for: ${provider}"
  kubectl wait "function.pkg.crossplane.io/${provider}" \
    --for=condition=Healthy --timeout="${TIMEOUT_PROVIDERS}" 2>/dev/null \
  || kubectl wait "provider.pkg.crossplane.io/${provider}" \
    --for=condition=Healthy --timeout="${TIMEOUT_PROVIDERS}" \
  || warn "  ${provider} not Healthy yet — continuing (may succeed later)"
done

# ── Step 2: Apply ProviderConfigs ───────────────────────────────────────────

info "Applying ProviderConfigs..."
kubectl apply -f "${REPO_ROOT}/providers/provider-configs.yaml"

# ── Step 3: Apply XRD and Composition ───────────────────────────────────────

info "Applying XRD (XCluster / Cluster)..."
kubectl apply -f "${REPO_ROOT}/xrds/cluster.yaml"

info "Waiting for XRD to be Established..."
kubectl wait xrd xclusters.gentianos.io \
  --for=condition=Established --timeout=2m

info "Applying Composition (cluster-default)..."
kubectl apply -f "${REPO_ROOT}/compositions/cluster-default.yaml"

# ── Step 4: Apply dev-cluster Claim ─────────────────────────────────────────

info "Checking that master-password Secret exists..."
kubectl get secret gentian-os-master-password -n crossplane-system >/dev/null 2>&1 \
  || fail "Secret 'gentian-os-master-password' not found in crossplane-system.
           Create it first:
             kubectl create secret generic gentian-os-master-password \\
               -n crossplane-system \\
               --from-literal=password=<your-master-password>"

info "Applying dev-cluster Cluster claim..."
# The static dev-cluster.yaml (domain: desk.gentian.org) is used for e2e tests.
# Production installs use crossplane/claims/dev-cluster.yaml.tmpl via install.sh.
kubectl apply -f "${REPO_ROOT}/claims/dev-cluster.yaml"

# ── Step 5: Wait for XCluster to be Ready ───────────────────────────────────

info "Waiting for XCluster dev-cluster to be Ready (timeout: ${TIMEOUT_XR})..."
kubectl wait xcluster/dev-cluster \
  --for=condition=Ready --timeout="${TIMEOUT_XR}" \
  || fail "XCluster dev-cluster did not become Ready within ${TIMEOUT_XR}. Check:
           kubectl describe xcluster dev-cluster
           kubectl get managed -l crossplane.io/composite=dev-cluster"

pass "XCluster dev-cluster is Ready"

# ── Step 6: Spot-check managed resources ───────────────────────────────────

info "Spot-checking managed resources..."
ERRORS=0

check_object() {
  local kind="$1" name="$2"
  if kubectl get "${kind}" "${name}" >/dev/null 2>&1; then
    pass "  ${kind}/${name} found"
  else
    warn "  ${kind}/${name} NOT found"
    ERRORS=$((ERRORS + 1))
  fi
}

check_k8s_object() {
  local kind="$1" name="$2" ns="${3:-}"
  local flags=""
  [[ -n "${ns}" ]] && flags="-n ${ns}"
  # shellcheck disable=SC2086  # flags is intentionally word-split (empty or "-n ns")
  if kubectl get "${kind}" "${name}" ${flags} >/dev/null 2>&1; then
    pass "  ${kind}/${name} found"
  else
    warn "  ${kind}/${name} NOT found"
    ERRORS=$((ERRORS + 1))
  fi
}

# Namespaces
check_k8s_object namespace platform-kernel
check_k8s_object namespace gentian-system

# ArgoCD AppProject
check_k8s_object appproject.argoproj.io gentianos-tenants argocd

# ESO ClusterSecretStore
check_k8s_object clustersecretstore.external-secrets.io openbao

# cert-manager ClusterIssuer
check_k8s_object clusterissuer.cert-manager.io letsencrypt-http01

# KV Seed SecretV2 resources (via Crossplane MR names)
check_object secretv2.kv.vault.upbound.io dev-cluster-kv-database-postgresql
check_object secretv2.kv.vault.upbound.io dev-cluster-kv-cache-redis

if [[ "${ERRORS}" -gt 0 ]]; then
  fail "${ERRORS} spot-check(s) failed — see warnings above"
fi

pass "All P1 spot-checks passed"
echo ""
echo "Phase 1 E2E complete. Kernel structural resources are provisioned."

