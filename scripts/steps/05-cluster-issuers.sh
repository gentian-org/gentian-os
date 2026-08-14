#!/usr/bin/env bash
# step: 05-cluster-issuers
# requires: 04-cert-manager
# provides: ClusterIssuers for the cluster's trust anchor
# mutates: cluster-scoped ClusterIssuer objects, a root CA Certificate under self-signed

# Dispatches on the trust anchor the cluster declares (§9). The default is
# public ACME, which is what a cluster with public DNS wants; a cluster on an
# internal domain has no reachable ACME endpoint and must say so, or nothing is
# ever issued and every Gateway listener sits at ResolvedRefs=False with a
# message about a missing Secret.

_issuer_mode() {
    # The claim is authoritative once it exists; the env var is how the
    # installer carries the answer before that.
    local mode
    mode="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.certificates.issuerMode}' 2>/dev/null || true)"
    echo "${mode:-${CERT_ISSUER_MODE:-acme-dns01}}"
}

check() {
    local mode; mode="$(_issuer_mode)"
    case "${mode}" in
        self-signed)
            kubectl get clusterissuer gentian-ca >/dev/null 2>&1
            ;;
        acme-dns01|acme-http01)
            [[ -n "$(kubectl get clusterissuer -o name 2>/dev/null | grep letsencrypt || true)" ]]
            ;;
        private-ca)
            kubectl get clusterissuer gentian-ca >/dev/null 2>&1
            ;;
        *)  return 1 ;;
    esac
}

apply() {
    local mode; mode="$(_issuer_mode)"
    info "Trust anchor: ${mode}"

    case "${mode}" in
        acme-dns01|acme-http01)
            apply_gentian_cluster_issuers
            ;;
        self-signed)
            gentian_run kubectl apply -f \
                "${SCRIPT_DIR}/kernel/manifests/cert-manager/cluster-issuers-selfsigned.yaml"
            info "Waiting for the root CA to be issued (up to 2m)..."
            kubectl wait --for=condition=Ready certificate/gentian-root-ca \
                -n cert-manager --timeout=120s ||
                warn "gentian-root-ca not Ready yet; gentian-ca will not issue until it is."
            warn "Certificates from this anchor are not publicly trusted."
            warn "  Import cert-manager/gentian-root-ca-tls tls.crt into any client"
            warn "  that validates kernel hostnames from outside the cluster."
            ;;
        private-ca)
            # An operator-supplied CA: the Secret has to exist first, because
            # cert-manager's ca issuer has nothing to generate from.
            local ref="${CERT_CA_BUNDLE_SECRET:-gentian-root-ca-tls}"
            if ! kubectl get secret "${ref}" -n cert-manager >/dev/null 2>&1; then
                error "issuerMode is private-ca but Secret cert-manager/${ref} does not exist."
                error "  Create it from your CA's certificate and key, then re-run this step."
                return 1
            fi
            gentian_run kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: gentian-ca
  labels:
    app.kubernetes.io/managed-by: gentian-install
    gentianos.io/issuer-mode: private-ca
spec:
  ca:
    secretName: ${ref}
EOF
            ;;
        *)
            # Never fall back to ACME. A cluster that asked for an offline
            # anchor and silently got a public one fails later, further away,
            # and for a reason that looks unrelated.
            error "Unknown certificates.issuerMode: ${mode}"
            error "  Supported: acme-dns01, acme-http01, private-ca, self-signed."
            return 1
            ;;
    esac
}

destroy() {
    kubectl delete clusterissuer -l gentianos.io/issuer-mode \
        --ignore-not-found=true 2>/dev/null || true
    kubectl delete certificate gentian-root-ca -n cert-manager \
        --ignore-not-found=true 2>/dev/null || true
    kubectl delete clusterissuer --all --ignore-not-found=true 2>/dev/null || true
}
