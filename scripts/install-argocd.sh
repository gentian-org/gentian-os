#!/bin/bash
# SPDX-FileCopyrightText: 2026 Gentian Organization
# SPDX-License-Identifier: AGPL-3.0-or-later

set -euo pipefail

ARGOCD_VERSION="${ARGOCD_VERSION:-v2.11.3}"
ARGOCD_NAMESPACE="argocd"

echo "Installing ArgoCD ${ARGOCD_VERSION}..."

# Wait for any pre-existing terminating namespace to fully disappear before
# recreating it.  A previous install run may have left the namespace in
# "Terminating" state; applying into it results in Forbidden errors.
if kubectl get namespace "${ARGOCD_NAMESPACE}" --request-timeout=5s \
        -o jsonpath='{.status.phase}' 2>/dev/null | grep -q "Terminating"; then
    echo "Namespace ${ARGOCD_NAMESPACE} is Terminating — force-finalizing..."
    # Same force-finalize pattern as uninstall.sh: clear spec.finalizers so the
    # API server removes the namespace immediately without waiting for GC.
    kubectl get namespace "${ARGOCD_NAMESPACE}" -o json --request-timeout=10s 2>/dev/null \
        | jq '.spec.finalizers=[]' \
        | kubectl replace --raw "/api/v1/namespaces/${ARGOCD_NAMESPACE}/finalize" -f - \
        2>/dev/null || true
    _t=$((SECONDS + 20))
    while kubectl get namespace "${ARGOCD_NAMESPACE}" --request-timeout=5s >/dev/null 2>&1; do
        (( SECONDS > _t )) && break
        sleep 2
    done
    echo "Namespace ${ARGOCD_NAMESPACE} gone."
fi

# Create namespace
echo "Creating namespace ${ARGOCD_NAMESPACE}..."
kubectl create namespace "${ARGOCD_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# Install ArgoCD
echo "Installing ArgoCD components..."
kubectl apply -n "${ARGOCD_NAMESPACE}" -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"

# NOTE: controller memory requests and concurrency limits are applied by
# tune_argocd_runtime() in scripts/lib/argocd.sh, which runs on EVERY install.sh
# run. Setting them here would only ever reach freshly-installed clusters,
# because install_argocd skips this script when argocd-server already exists.

# Wait for ArgoCD to be ready
echo "Waiting for ArgoCD server to be ready..."
kubectl wait --for=condition=available --timeout=300s \
  deployment/argocd-server -n "${ARGOCD_NAMESPACE}"

kubectl wait --for=condition=available --timeout=300s \
  deployment/argocd-repo-server -n "${ARGOCD_NAMESPACE}"

kubectl wait --for=condition=available --timeout=300s \
  deployment/argocd-applicationset-controller -n "${ARGOCD_NAMESPACE}"

kubectl rollout status statefulset/argocd-application-controller -n "${ARGOCD_NAMESPACE}" --timeout=300s

# Expose ArgoCD server via ClusterIP
echo "Configuring ArgoCD server as ClusterIP..."
kubectl patch svc argocd-server -n "${ARGOCD_NAMESPACE}" -p '{
  "spec": {
    "type": "ClusterIP",
    "ports": [
      {"name": "http",  "port": 80,  "targetPort": 8080},
      {"name": "https", "port": 443, "targetPort": 8080}
    ]
  }
}'

echo ""
echo "ArgoCD installed successfully!"
echo ""
echo "To access ArgoCD:"
echo "1. Get the initial admin password:"
echo "   kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath=\"{.data.password}\" | base64 -d && echo"
echo ""
echo "2. Access the UI via port-forward:"
echo "   kubectl port-forward -n argocd svc/argocd-server 8080:443"
echo "   Then open: https://localhost:8080"
echo "   (Note: You may need to accept the self-signed certificate)"
echo ""
echo "3. Login with:"
echo "   Username: admin"
echo "   Password: <from step 1>"
echo ""
echo "4. (Optional) Install ArgoCD CLI:"
echo "   curl -sSL -o argocd-linux-amd64 https://github.com/argoproj/argo-cd/releases/download/${ARGOCD_VERSION}/argocd-linux-amd64"
echo "   sudo install -m 555 argocd-linux-amd64 /usr/local/bin/argocd"
echo "   rm argocd-linux-amd64"
echo ""
