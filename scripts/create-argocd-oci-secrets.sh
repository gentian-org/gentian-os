#!/bin/bash
set -e

# Create ArgoCD repository secrets for OCI Helm charts that still pull from
# registry.opencode.de. Infra step 1 removed opencode for redis/minio (public
# Bitnami) and postgres/mariadb (vendored classic repo on GitHub raw).
#
# Usage: ./create-argocd-oci-secrets.sh <username> <password>

USERNAME=$1
PASSWORD=$2

if [ -z "$USERNAME" ] || [ -z "$PASSWORD" ]; then
    echo "Usage: $0 <username> <password>"
    echo "Example: $0 \"\$OD_PRIVATE_REGISTRY_USERNAME\" \"\$OD_PRIVATE_REGISTRY_PASSWORD\""
    exit 1
fi

NAMESPACE="argocd"

echo "Creating ArgoCD repository secrets for OCI Helm charts in namespace: $NAMESPACE"

# Create univention-charts repository secret (for Nubus and Intercom Service)
kubectl create secret generic univention-charts -n "$NAMESPACE" \
  --from-literal=type=helm \
  --from-literal=name=univention-charts \
  --from-literal=url=registry.opencode.de/bmi/opendesk/components/supplier/univention/charts-mirror \
  --from-literal=enableOCI=true \
  --from-literal=username="$USERNAME" \
  --from-literal=password="$PASSWORD" \
  --dry-run=client -o yaml | \
  kubectl label -f - argocd.argoproj.io/secret-type=repository --local --dry-run=client -o yaml | \
  kubectl apply -f -

echo "✅ Created: univention-charts"

# Create opendesk-keycloak-bootstrap repository secret
kubectl create secret generic opendesk-keycloak-bootstrap-charts -n "$NAMESPACE" \
  --from-literal=type=helm \
  --from-literal=name=opendesk-keycloak-bootstrap \
  --from-literal=url=registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-keycloak-bootstrap \
  --from-literal=enableOCI=true \
  --from-literal=username="$USERNAME" \
  --from-literal=password="$PASSWORD" \
  --dry-run=client -o yaml | \
  kubectl label -f - argocd.argoproj.io/secret-type=repository --local --dry-run=client -o yaml | \
  kubectl apply -f -

echo "✅ Created: opendesk-keycloak-bootstrap-charts"

echo ""
echo "ArgoCD repository secrets created successfully!"
echo ""
echo "Verify with:"
echo "  kubectl get secrets -n argocd -l argocd.argoproj.io/secret-type=repository"
echo ""
echo "Note: redis/minio use public oci://registry-1.docker.io/bitnamicharts (no secret)."
echo "      postgres/mariadb use vendored charts at charts/infra/packages/ (no secret)."
