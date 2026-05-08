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
if kubectl get namespace "${ARGOCD_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null | grep -q "Terminating"; then
    echo "Namespace ${ARGOCD_NAMESPACE} is Terminating — waiting up to 120 s for deletion..."
    kubectl wait --for=delete namespace/"${ARGOCD_NAMESPACE}" --timeout=120s 2>/dev/null || true
    echo "Namespace ${ARGOCD_NAMESPACE} gone."
fi

# Create namespace
echo "Creating namespace ${ARGOCD_NAMESPACE}..."
kubectl create namespace "${ARGOCD_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# Install ArgoCD
echo "Installing ArgoCD components..."
kubectl apply -n "${ARGOCD_NAMESPACE}" -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"

# Wait for ArgoCD to be ready
echo "Waiting for ArgoCD server to be ready..."
kubectl wait --for=condition=available --timeout=300s \
  deployment/argocd-server -n "${ARGOCD_NAMESPACE}"

kubectl wait --for=condition=available --timeout=300s \
  deployment/argocd-repo-server -n "${ARGOCD_NAMESPACE}"

kubectl wait --for=condition=available --timeout=300s \
  deployment/argocd-applicationset-controller -n "${ARGOCD_NAMESPACE}"

# Expose ArgoCD server via NodePort with static ports
echo "Configuring ArgoCD server as NodePort (HTTP: 30880, HTTPS: 30443)..."
kubectl patch svc argocd-server -n "${ARGOCD_NAMESPACE}" -p '{
  "spec": {
    "type": "NodePort",
    "ports": [
      {"name": "http",  "port": 80,  "targetPort": 8080, "nodePort": 30880},
      {"name": "https", "port": 443, "targetPort": 8080, "nodePort": 30443}
    ]
  }
}'

NODEPORT=30443

echo ""
echo "ArgoCD installed successfully!"
echo ""
echo "To access ArgoCD:"
echo "1. Get the initial admin password:"
echo "   kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath=\"{.data.password}\" | base64 -d && echo"
echo ""
echo "2. Access the UI at:"
echo "   https://<node-ip>:${NODEPORT}  (HTTP: http://<node-ip>:30880)"
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
