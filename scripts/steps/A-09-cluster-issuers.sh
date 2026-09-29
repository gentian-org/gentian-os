#!/usr/bin/env bash
# step: A-09-cluster-issuers
# phase: control-plane
# requires: A-02-cert-manager
# provides: the cluster's trust anchor as ClusterIssuers
# mutates: cluster-scoped ClusterIssuer objects, a root CA Certificate under self-signed

# v5 reached the issuers only through C-03's install_kernel_wildcard, which
# returns early when the DNS provider is `none`. So a cluster without DNS-01
# got no ClusterIssuers at all -- not even the HTTP-01 one it can actually
# use -- and the two offline anchors, self-signed and private-ca, had no v5
# path whatsoever. The dispatch is the step; C-03 keeps the wildcard.
#
# Runs before the Gateway (A-05 has already run by filename order, but nothing
# here needs it): a ClusterIssuer names its solver's Gateway without resolving
# it, and the trust anchor has to exist before anything asks for a certificate.

# The cert-manager namespace, said once. v5 puts cert-manager in the edge
# namespace; detection is the fallback for a cluster where it came from a
# distro addon, and gentian_cert_manager_namespace already does that -- it
# only trusts CERT_MANAGER_NAMESPACE when a webhook actually answers there.
_v5_cert_manager_namespace() {
    CERT_MANAGER_NAMESPACE="${CERT_MANAGER_NAMESPACE:-$(ns_kernel edge)}"
    export CERT_MANAGER_NAMESPACE
    CERT_MANAGER_NAMESPACE="$(gentian_cert_manager_namespace)"
    export CERT_MANAGER_NAMESPACE
    echo "${CERT_MANAGER_NAMESPACE}"
}

# The claim is authoritative once it exists; the env var is how the installer
# carries the answer before C-01 has created it.
_v5_issuer_mode() {
    local mode
    mode="$(kubectl get cluster.gentianos.io -n "$(ns_kernel provisioning)" \
        -o jsonpath='{.items[0].spec.certificates.issuerMode}' 2>/dev/null || true)"
    echo "${mode:-${CERT_ISSUER_MODE:-acme-dns01}}"
}

# The HTTP-01 issuer this cluster's ACME environment names. Checking for "some
# letsencrypt issuer" stayed satisfied on a cluster whose intent had moved to
# staging while the staging issuers were never applied; the name is the
# contract between this step and every Certificate, so check the name.
_v5_http01_issuer_name() {
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        echo "letsencrypt-staging-http01"
    else
        echo "letsencrypt-http01"
    fi
}

check() {
    # Without a kernel domain there is no ACME identifier to ask about and
    # nothing for this step to apply -- which is not the same as unsatisfied.
    [[ -n "${KERNEL_DOMAIN:-}" ]] || return "${CHECK_UNDEFINED}"

    local mode; mode="$(_v5_issuer_mode)"
    case "${mode}" in
        self-signed)
            # Both objects apply() creates. The ClusterIssuer alone existing
            # does not mean it can still issue: it signs from
            # gentian-root-ca-tls, which only the Certificate keeps current,
            # and destroy() removes the two separately.
            kubectl get clusterissuer gentian-ca >/dev/null 2>&1 || return "${CHECK_MISSING}"
            kubectl get certificate gentian-root-ca -n "$(_v5_cert_manager_namespace)" >/dev/null 2>&1 ||
                return "${CHECK_MISSING}"
            ;;
        acme-dns01|acme-http01)
            kubectl get clusterissuer "$(_v5_http01_issuer_name)" >/dev/null 2>&1 ||
                return "${CHECK_MISSING}"
            if [[ "$(gentian_dns_provider)" != "none" ]]; then
                kubectl get clusterissuer "$(gentian_dns01_cluster_issuer_name)" >/dev/null 2>&1 ||
                    return "${CHECK_MISSING}"
            fi
            ;;
        private-ca)
            kubectl get clusterissuer gentian-ca >/dev/null 2>&1 || return "${CHECK_MISSING}"
            ;;
        *)  return "${CHECK_MISSING}" ;;
    esac
    return "${CHECK_SATISFIED}"
}

apply() {
    banner "Cluster issuers"

    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset: no trust anchor to apply."
        return 0
    fi

    local mode ns
    mode="$(_v5_issuer_mode)"
    info "Trust anchor: ${mode}"

    # Every mode needs the webhook up first: cert-manager validates
    # ClusterIssuer admission through it, and applying one before it is Ready
    # fails with an admission error that names the webhook rather than the
    # issuer.
    ns="$(_v5_cert_manager_namespace)"
    if ! kubectl get deploy cert-manager-webhook -n "${ns}" >/dev/null 2>&1; then
        error "cert-manager webhook not found in ${ns} or anywhere else."
        error "  Fix cert-manager first (A-02-cert-manager), then re-run this step."
        return 1
    fi
    info "Waiting for the cert-manager webhook in ${ns}..."
    kubectl rollout status -n "${ns}" deploy/cert-manager-webhook --timeout=180s >/dev/null ||
        warn "cert-manager-webhook not Ready within 180s; issuer admission may fail."

    case "${mode}" in
        acme-dns01|acme-http01)
            # The gateway the HTTP-01 solver routes through lives in the edge
            # namespace on v5, not in v4's platform-kernel.
            export KERNEL_PUBLIC_GATEWAY_NAMESPACE="${KERNEL_PUBLIC_GATEWAY_NAMESPACE:-$(ns_kernel edge)}"
            apply_gentian_cluster_issuers
            if [[ "$(gentian_dns_provider)" == "none" ]]; then
                success "ClusterIssuer $(_v5_http01_issuer_name) applied."
                info "  No DNS provider: this cluster issues per-host certificates, not wildcards."
            else
                success "ClusterIssuers $(_v5_http01_issuer_name) and $(gentian_dns01_cluster_issuer_name) applied."
                info "  The DNS-01 issuer stays NotReady until C-03 materialises its credential."
            fi
            ;;
        self-signed)
            gentian_run kubectl apply -n "${ns}" -f \
                "${SCRIPT_DIR}/kernel/manifests/cert-manager/cluster-issuers-selfsigned.yaml"
            info "Waiting for the root CA to be issued (up to 2m)..."
            kubectl wait --for=condition=Ready "certificate/gentian-root-ca" \
                -n "${ns}" --timeout=120s >/dev/null ||
                warn "gentian-root-ca not Ready yet; gentian-ca will not issue until it is."
            warn "Certificates from this anchor are not publicly trusted."
            warn "  Import ${ns}/gentian-root-ca-tls tls.crt into any client that"
            warn "  validates kernel hostnames from outside the cluster."
            ;;
        private-ca)
            # An operator-supplied CA: the Secret has to exist first, because
            # cert-manager's ca issuer has nothing to generate from.
            local ref="${CERT_CA_BUNDLE_SECRET:-gentian-root-ca-tls}"
            if ! kubectl get secret "${ref}" -n "${ns}" >/dev/null 2>&1; then
                error "issuerMode is private-ca but Secret ${ns}/${ref} does not exist."
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
    # Only the issuers this installer labelled, and the root CA Certificate
    # that has no label of its own. A ClusterIssuer somebody else put on this
    # cluster is not ours to remove -- v4's step ended with a
    # `delete clusterissuer --all`, which on a shared cluster takes out
    # everyone else's anchors too.
    kubectl delete clusterissuer -l app.kubernetes.io/managed-by=gentian-install \
        --ignore-not-found=true >/dev/null 2>&1 || true
    kubectl delete certificate gentian-root-ca -n "$(_v5_cert_manager_namespace)" \
        --ignore-not-found=true >/dev/null 2>&1 || true
}
