#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

echo "Bootstrapping ArgoCD root ApplicationSet..."

# Apply the root ApplicationSet
kubectl apply -f "${REPO_ROOT}/kernel/bootstrap/root-applicationset.yaml"

echo ""
echo "✅ Root ApplicationSet deployed!"
echo ""
echo "The root ApplicationSet will automatically discover and deploy"
echo "all ApplicationSets in the applicationsets/ directory."
echo ""
echo "Monitor deployment:"
echo "  kubectl get applicationsets -n argocd"
echo "  kubectl get applications -n argocd"
echo ""
echo "Access ArgoCD UI to view the app-of-apps pattern in action."
