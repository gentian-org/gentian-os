#!/usr/bin/env bash
# Create gentian-staging-ca-tls in the services namespace for ACME staging dev
# clusters. Pods that call https://id.<kernel-domain> need the Let's Encrypt
# staging intermediate in their trust store (opendesk certificate.selfSigned parity).
#
# Usage: create-staging-ca-secret.sh [namespace]
# Default namespace: gentian-dev (servicesNamespace).
set -euo pipefail

NAMESPACE="${1:-gentian-dev}"
SECRET_NAME="gentian-staging-ca-tls"
CERT_NS="${CERT_MANAGER_NS:-cert-manager}"
LEAF_SECRET="${KERNEL_TLS_SECRET:-wildcard-kernel-tls}"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

if ! kubectl get secret "$LEAF_SECRET" -n "$CERT_NS" &>/dev/null; then
  echo "skip: $LEAF_SECRET not found in $CERT_NS (not an ACME staging cluster?)" >&2
  exit 0
fi

kubectl get secret "$LEAF_SECRET" -n "$CERT_NS" -o jsonpath='{.data.tls\.crt}' \
  | base64 -d >"$TMPDIR/leaf.pem"

# System CA bundle (Alpine ships a current Mozilla bundle).
docker run --rm alpine:3.20 cat /etc/ssl/certs/ca-certificates.crt >"$TMPDIR/bundle.crt" 2>/dev/null \
  || cp /etc/ssl/certs/ca-certificates.crt "$TMPDIR/bundle.crt"

# Append the issuing intermediate via AIA when present (LE staging chain).
AIA="$(openssl x509 -in "$TMPDIR/leaf.pem" -noout -text 2>/dev/null \
  | awk -F'URI:' '/CA Issuers - URI:/{print $2; exit}' | tr -d '[:space:]')" || true
if [[ -n "$AIA" ]]; then
  curl -fsSL "$AIA" -o "$TMPDIR/intermediate.der"
  # Let's Encrypt AIA serves DER; append as PEM so awk/keytool can import it.
  openssl x509 -inform DER -in "$TMPDIR/intermediate.der" -out "$TMPDIR/intermediate.pem"
  printf '\n' >>"$TMPDIR/bundle.crt"
  cat "$TMPDIR/intermediate.pem" >>"$TMPDIR/bundle.crt"
  echo "appended intermediate from $AIA"
else
  echo "warning: no AIA on $LEAF_SECRET leaf; bundle is system CAs only" >&2
fi

# Java OIDC clients (XWiki, etc.) need a JKS truststore; curl/python use ca.crt.
# keytool -importcert on a multi-cert PEM only imports the first certificate.
awk '/BEGIN CERT/{n++}{print > "'"$TMPDIR"'/cert-" n ".pem"}' "$TMPDIR/bundle.crt"
docker run --rm -v "$TMPDIR:/work" eclipse-temurin:17-jdk bash -ec '
  rm -f /work/truststore.jks
  n=0
  for cert in /work/cert-*.pem; do
    [[ -s "$cert" ]] || continue
    keytool -importcert -noprompt -alias "staging-ca-${n}" \
      -file "$cert" -keystore /work/truststore.jks -storetype JKS -storepass changeit
    n=$((n + 1))
  done
  test -f /work/truststore.jks
'

if kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" &>/dev/null; then
  kubectl delete secret "$SECRET_NAME" -n "$NAMESPACE"
fi
kubectl create secret generic "$SECRET_NAME" \
  --namespace="$NAMESPACE" \
  --from-file=ca.crt="$TMPDIR/bundle.crt" \
  --from-file=truststore.jks="$TMPDIR/truststore.jks"

echo "secret/$SECRET_NAME updated in namespace $NAMESPACE (ca.crt + truststore.jks)"
