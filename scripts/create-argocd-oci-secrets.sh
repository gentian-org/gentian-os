#!/bin/bash
set -e

# Script to create ArgoCD repository secrets for OCI Helm charts
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

# Create opendesk-postgresql repository secret
kubectl create secret generic opendesk-postgresql-charts -n "$NAMESPACE" \
  --from-literal=type=helm \
  --from-literal=name=opendesk-postgresql \
  --from-literal=url=registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-postgresql \
  --from-literal=enableOCI=true \
  --from-literal=username="$USERNAME" \
  --from-literal=password="$PASSWORD" \
  --dry-run=client -o yaml | \
  kubectl label -f - argocd.argoproj.io/secret-type=repository --local --dry-run=client -o yaml | \
  kubectl apply -f -

echo "✅ Created: opendesk-postgresql-charts"

# Create opendesk-mariadb repository secret
kubectl create secret generic opendesk-mariadb-charts -n "$NAMESPACE" \
  --from-literal=type=helm \
  --from-literal=name=opendesk-mariadb \
  --from-literal=url=registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-mariadb \
  --from-literal=enableOCI=true \
  --from-literal=username="$USERNAME" \
  --from-literal=password="$PASSWORD" \
  --dry-run=client -o yaml | \
  kubectl label -f - argocd.argoproj.io/secret-type=repository --local --dry-run=client -o yaml | \
  kubectl apply -f -

echo "✅ Created: opendesk-mariadb-charts"

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

# Create bitnami-charts repository secret (for MinIO, Redis)
kubectl create secret generic bitnami-charts -n "$NAMESPACE" \
  --from-literal=type=helm \
  --from-literal=name=bitnami-charts \
  --from-literal=url=registry.opencode.de/bmi/opendesk/components/external/charts/bitnami-charts \
  --from-literal=enableOCI=true \
  --from-literal=username="$USERNAME" \
  --from-literal=password="$PASSWORD" \
  --dry-run=client -o yaml | \
  kubectl label -f - argocd.argoproj.io/secret-type=repository --local --dry-run=client -o yaml | \
  kubectl apply -f -

echo "✅ Created: bitnami-charts"

echo ""
echo "ArgoCD repository secrets created successfully!"
echo ""
echo "Verify with:"
echo "  kubectl get secrets -n argocd -l argocd.argoproj.io/secret-type=repository"
echo ""
echo "Note: These secrets enable ArgoCD to authenticate to private OCI registries."
echo "The repo-server deployment will automatically pick up these credentials."
