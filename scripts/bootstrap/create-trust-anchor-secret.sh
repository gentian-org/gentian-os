#!/usr/bin/env bash
# Create gentian-trust-anchor-tls in the services namespace for ACME staging dev
# clusters. Pods that call https://id.<kernel-domain> need the Let's Encrypt
# staging intermediate in their trust store (self-signed CA parity).
#
# Usage: create-trust-anchor-secret.sh [namespace]
# Default namespace: ${SERVICES_NAMESPACE:-gentian-${ENV:-dev}}
set -euo pipefail

ENV="${ENV:-dev}"
NAMESPACE="${1:-${SERVICES_NAMESPACE:-gentian-${ENV}}}"
SECRET_NAME="gentian-trust-anchor-tls"
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

# Walk the LE staging issuer chain via AIA (intermediate → root → …).
# Node.js NODE_EXTRA_CA_CERTS needs the full chain, not just the first intermediate.
AIA="$(openssl x509 -in "$TMPDIR/leaf.pem" -noout -text 2>/dev/null \
  | awk -F'URI:' '/CA Issuers - URI:/{print $2; exit}' | tr -d '[:space:]')" || true
: >"$TMPDIR/node-extra-ca.crt"
step=0
while [[ -n "$AIA" && "$step" -lt 8 ]]; do
  curl -fsSL "$AIA" -o "$TMPDIR/chain.der"
  openssl x509 -inform DER -in "$TMPDIR/chain.der" -out "$TMPDIR/chain.pem"
  printf '\n' >>"$TMPDIR/bundle.crt"
  cat "$TMPDIR/chain.pem" >>"$TMPDIR/bundle.crt"
  printf '\n' >>"$TMPDIR/node-extra-ca.crt"
  cat "$TMPDIR/chain.pem" >>"$TMPDIR/node-extra-ca.crt"
  echo "appended issuer from $AIA"
  subj="$(openssl x509 -in "$TMPDIR/chain.pem" -noout -subject 2>/dev/null || true)"
  issuer="$(openssl x509 -in "$TMPDIR/chain.pem" -noout -issuer 2>/dev/null || true)"
  if [[ "$subj" == "$issuer" ]]; then
    break
  fi
  AIA="$(openssl x509 -in "$TMPDIR/chain.pem" -noout -text 2>/dev/null \
    | awk -F'URI:' '/CA Issuers - URI:/{print $2; exit}' | tr -d '[:space:]')" || true
  step=$((step + 1))
done
if [[ ! -s "$TMPDIR/node-extra-ca.crt" ]]; then
  echo "warning: no AIA chain on $LEAF_SECRET leaf; bundle is system CAs only" >&2
fi

# Java OIDC clients (XWiki, etc.) need a JKS truststore; curl/python use ca.crt.
# keytool -importcert on a multi-cert PEM only imports the first certificate.
awk '/BEGIN CERT/{n++}{print > "'"$TMPDIR"'/cert-" n ".pem"}' "$TMPDIR/bundle.crt"
docker run --rm -v "$TMPDIR:/work" eclipse-temurin:17-jdk bash -ec '
  rm -f /work/truststore.jks
  n=0
  for cert in /work/cert-*.pem; do
    [[ -s "$cert" ]] || continue
    keytool -importcert -noprompt -alias "trust-anchor-${n}" \
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
  --from-file=node-extra-ca.crt="$TMPDIR/node-extra-ca.crt" \
  --from-file=truststore.jks="$TMPDIR/truststore.jks"

echo "secret/$SECRET_NAME updated in namespace $NAMESPACE (ca.crt + node-extra-ca.crt + truststore.jks)"
