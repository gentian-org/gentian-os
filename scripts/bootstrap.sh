#!/bin/bash
set -e

echo "Bootstrapping ArgoCD root ApplicationSet..."

# Apply the root ApplicationSet
kubectl apply -f argocd/bootstrap/root-applicationset.yaml

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
