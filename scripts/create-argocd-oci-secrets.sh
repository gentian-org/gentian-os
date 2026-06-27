#!/bin/bash
set -e

# Create ArgoCD repository secrets for private OCI Helm chart registries used by
# kernel services that still pull from registry.opencode.de (Nubus, Nextcloud, mail, …).
#
# Redis, MinIO, PostgreSQL, and MariaDB do not require these secrets.
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

# Univention chart mirror (Nubus, Intercom Service)
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

# Keycloak bootstrap chart registry credential (chart not deployed in current kernel)
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
