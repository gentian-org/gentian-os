#!/usr/bin/env bash
# P0 E2E — Install Crossplane core on the dev cluster and verify Ready.
# Usage: make e2e-p0
# Rollback: make e2e-p0-clean
set -euo pipefail

NAMESPACE=crossplane-system
CHART_VERSION="1.18.0"   # Crossplane Helm chart release; bump as needed
HELM_REPO=https://charts.crossplane.io/stable
TIMEOUT=5m

info()  { echo "[INFO]  $*"; }
pass()  { echo "[PASS]  $*"; }
fail()  { echo "[FAIL]  $*"; exit 1; }

# ── Pre-checks ──────────────────────────────────────────────────────────────

info "Checking prerequisites..."
command -v kubectl  >/dev/null 2>&1 || fail "kubectl not found"
command -v helm     >/dev/null 2>&1 || fail "helm not found"
command -v crossplane >/dev/null 2>&1 || fail "crossplane CLI not found (run: make install-tools)"

kubectl cluster-info --request-timeout=5s >/dev/null 2>&1 \
  || fail "No reachable cluster — point KUBECONFIG at your dev cluster."

info "Cluster reachable: $(kubectl config current-context)"

# ── Install Crossplane ───────────────────────────────────────────────────────
# Detect any existing Crossplane install (Helm-managed, microk8s addon, or
# kubectl-applied) by checking for the crossplane Deployment directly.

if kubectl get deployment crossplane -n "${NAMESPACE}" >/dev/null 2>&1; then
  info "Crossplane deployment already present in ${NAMESPACE}; skipping install."
elif helm status crossplane -n "${NAMESPACE}" >/dev/null 2>&1; then
  info "Crossplane already installed via Helm in ${NAMESPACE}; skipping install."
else
  # Clean up any orphaned cluster-scoped resources left by a previous failed
  # install (they lack Helm ownership labels and would block a fresh install).
  if kubectl get clusterrole crossplane >/dev/null 2>&1; then
    info "Removing orphaned Crossplane cluster-scoped resources before install..."
    kubectl delete clusterrole \
      crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
      --ignore-not-found=true
    kubectl delete clusterrolebinding \
      crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse \
      --ignore-not-found=true
    kubectl delete namespace "${NAMESPACE}" --ignore-not-found=true
  fi

  info "Adding Crossplane Helm repo..."
  helm repo add crossplane-stable "${HELM_REPO}" --force-update
  helm repo update

  info "Installing Crossplane ${CHART_VERSION} in namespace ${NAMESPACE}..."
  helm install crossplane crossplane-stable/crossplane \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --version "${CHART_VERSION}" \
    --set replicas=1 \
    --wait \
    --timeout "${TIMEOUT}"
fi

# ── Readiness checks ─────────────────────────────────────────────────────────

info "Waiting for Crossplane deployment to be Available..."
kubectl wait deployment/crossplane \
  -n "${NAMESPACE}" \
  --for=condition=Available \
  --timeout="${TIMEOUT}" \
  || fail "crossplane deployment did not become Available within ${TIMEOUT}"

info "Waiting for crossplane-rbac-manager deployment (if present)..."
kubectl wait deployment/crossplane-rbac-manager \
  -n "${NAMESPACE}" \
  --for=condition=Available \
  --timeout="${TIMEOUT}" 2>/dev/null || true

pass "crossplane pod is Ready:"
kubectl get pods -n "${NAMESPACE}"

# ── CRD smoke ────────────────────────────────────────────────────────────────

info "Verifying core Crossplane CRDs are registered..."
for crd in \
  compositeresourcedefinitions.apiextensions.crossplane.io \
  compositions.apiextensions.crossplane.io \
  functions.pkg.crossplane.io \
  providers.pkg.crossplane.io; do
  if kubectl get crd "${crd}" >/dev/null 2>&1; then
    pass "CRD present: ${crd}"
  else
    fail "CRD missing:  ${crd}"
  fi
done

# ── crossplane CLI health ─────────────────────────────────────────────────────

info "Checking crossplane CLI version..."
crossplane version || true   # exits 1 when no server-side component matches; CLI itself is OK

# ── Summary ──────────────────────────────────────────────────────────────────

echo ""
echo "╔═══════════════════════════════════════════════╗"
echo "║  P0 E2E RESULT: PASS                          ║"
echo "║  Crossplane core is installed and Ready.      ║"
echo "║  Run 'make e2e-p0-clean' to uninstall.        ║"
echo "╚═══════════════════════════════════════════════╝"
