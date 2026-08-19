#!/usr/bin/env bash
# step: A-06-cluster-issuers
# phase: control-plane
# requires: A-05-cert-manager
# provides: ClusterIssuers for the cluster's trust anchor
# mutates: cluster-scoped ClusterIssuer objects, a root CA Certificate under self-signed

# Dispatches on the trust anchor the cluster declares (§9). The default is
# public ACME, which is what a cluster with public DNS wants; a cluster on an
# internal domain has no reachable ACME endpoint and must say so, or nothing is
# ever issued and every Gateway listener sits at ResolvedRefs=False with a
# message about a missing Secret.

# _wait_for_cert_manager_webhook — shared by every mode.
#
# The namespace is detected rather than assumed: a cluster where cert-manager
# came from a distro addon puts the webhook somewhere else, and applying an
# issuer against the wrong namespace fails in a way that reads as a cert-manager
# fault rather than a lookup one.
_wait_for_cert_manager_webhook() {
    local ns="${CERT_MANAGER_NAMESPACE:-cert-manager}"
    if ! kubectl get deploy cert-manager-webhook -n "${ns}" >/dev/null 2>&1; then
        local detected
        detected="$(kubectl get deploy -A -o json 2>/dev/null |
            jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' |
            head -1 || true)"
        [[ -n "${detected}" ]] && ns="${detected}"
    fi
    CERT_MANAGER_NAMESPACE="${ns}"
    export CERT_MANAGER_NAMESPACE

    if ! kubectl get deploy cert-manager-webhook -n "${ns}" >/dev/null 2>&1; then
        error "cert-manager webhook not found in any namespace."
        error "  Fix cert-manager first (step 04), then re-run this step."
        return 1
    fi
    info "Waiting for the cert-manager webhook in ${ns}..."
    kubectl rollout status -n "${ns}" deploy/cert-manager-webhook --timeout=180s >/dev/null ||
        warn "cert-manager-webhook not Ready within 180s; issuer admission may fail."
}

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
            # The issuers this cluster's ACME endpoint NAMES, not any
            # letsencrypt-*. "Some issuer exists" stayed satisfied on a cluster
            # installed against production after the intent moved to staging —
            # the staging issuers were never applied, every tenant wildcard
            # Certificate pointed at a ClusterIssuer that did not exist, and the
            # symptom surfaced three layers away as a Gateway listener with no
            # certificate and a 404 on the tenant's hosts. The name is the
            # contract between this step and everything that requests
            # certificates; check the name.
            local http01_name="letsencrypt-http01"
            [[ "${ACME_ENV:-production}" == "staging" ]] && http01_name="letsencrypt-staging-http01"
            kubectl get clusterissuer "${http01_name}" >/dev/null 2>&1 || return 1
            if [[ "$(gentian_dns_provider)" != "none" ]]; then
                kubectl get clusterissuer "$(gentian_dns01_cluster_issuer_name)" >/dev/null 2>&1 || return 1
            fi
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

    # Every mode needs the webhook up first: cert-manager validates ClusterIssuer
    # admission through it, so applying one before it is Ready fails with an
    # admission error that names the webhook rather than the issuer.
    _wait_for_cert_manager_webhook

    case "${mode}" in
        acme-dns01|acme-http01)
            # install_kernel_cert_resources rather than apply_gentian_cluster_issuers:
            # it also honours INSTALL_CLUSTER_INFRA and an unset KERNEL_DOMAIN,
            # and reports which issuer set landed.
            install_kernel_cert_resources
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
