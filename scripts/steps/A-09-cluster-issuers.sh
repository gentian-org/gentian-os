#!/usr/bin/env bash
# step: A-09-cluster-issuers
# phase: control-plane
# requires: A-02-cert-manager A-03-external-secrets
# provides: the cluster's trust anchor as ClusterIssuers, and the internal CA gentian-ca on every cluster
# mutates: cluster-scoped ClusterIssuer objects; the internal CA's ServiceAccount, and the Role and RoleBinding that let cert-manager request its token, in the cert-manager namespace

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

_v5_issuer_mode() { gentian_issuer_mode; }

# The internal CA, gentian-ca, is on every cluster
# (kernel/manifests/cert-manager/internal-ca.yaml): a ClusterIssuer that asks
# the vault's PKI engine to sign, and the ServiceAccount it logs in to the
# vault as. On a private-ca cluster gentian-ca is the person's own CA instead,
# applied below as before.
_v5_internal_ca_manifest() {
    printf '%s/kernel/manifests/cert-manager/internal-ca.yaml' "${SCRIPT_DIR}"
}

# The manifest with this cluster's cert-manager namespace and vault address.
_v5_internal_ca_rendered() {
    sed -e "s|@CERT_MANAGER_NAMESPACE@|$(_v5_cert_manager_namespace)|g" \
        -e "s|@OPENBAO_SERVER@|https://openbao.$(ns_kernel secrets).svc.cluster.local:8200|g" \
        "$(_v5_internal_ca_manifest)"
}

# The ClusterIssuer must be the vault one: a gentian-ca left as a CA issuer
# signs with a key in a Secret, which is what this replaced.
_v5_internal_ca_present() {
    local ns; ns="$(_v5_cert_manager_namespace)"
    [[ "$(kubectl get clusterissuer gentian-ca -o jsonpath='{.spec.vault.path}' 2>/dev/null)" == \
        "pki-internal/sign/gentian-ca" ]] &&
        kubectl get serviceaccount gentian-ca-issuer -n "${ns}" >/dev/null 2>&1 &&
        kubectl get rolebinding gentian-ca-issuer-token -n "${ns}" >/dev/null 2>&1
}

# What held the internal CA's key in a Kubernetes object before the vault
# signed for it, retired so that no key is left there:
#   - on a self-signed cluster, the CA cert-manager made: a selfSigned issuer
#     signing a CA Certificate into gentian-root-ca-tls. The Certificate and
#     the issuer go here; the Secret stays until B-08 has imported its CA
#     into the vault, so the anchor clients were told to trust is kept, and
#     B-08 then rewrites it without the key.
#   - an ExternalSecret gentian-root-ca-tls that copied the CA's key out of
#     the vault's KV store. It owns its Secret, which goes with it.
_v5_retire_cert_manager_ca() {
    local ns; ns="$(_v5_cert_manager_namespace)"
    if kubectl get certificate gentian-root-ca -n "${ns}" >/dev/null 2>&1; then
        info "Retiring the CA cert-manager made; the internal CA is the vault's from now on."
        gentian_run kubectl delete certificate gentian-root-ca -n "${ns}" --ignore-not-found=true >/dev/null
    fi
    if kubectl get clusterissuer gentian-selfsigned-bootstrap >/dev/null 2>&1; then
        gentian_run kubectl delete clusterissuer gentian-selfsigned-bootstrap --ignore-not-found=true >/dev/null
    fi
    if kubectl get externalsecret gentian-root-ca-tls -n "${ns}" >/dev/null 2>&1; then
        gentian_run kubectl delete externalsecret gentian-root-ca-tls -n "${ns}" --ignore-not-found=true >/dev/null
    fi
}

_v5_apply_internal_ca() {
    local ns; ns="$(_v5_cert_manager_namespace)"
    _v5_retire_cert_manager_ca
    _v5_internal_ca_rendered | gentian_run kubectl apply -n "${ns}" -f -
    info "Internal CA gentian-ca applied; it issues once the vault has the CA (B-08) and"
    info "  cert-manager's role at the vault exists (C-01). It serves in-cluster TLS."
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
            # What apply() creates. A CA Certificate left from before the
            # vault signed is a CA beside it, and apply() retires it.
            _v5_internal_ca_present || return "${CHECK_MISSING}"
            kubectl get certificate gentian-root-ca -n "$(_v5_cert_manager_namespace)" >/dev/null 2>&1 &&
                return "${CHECK_MISSING}"
            ;;
        acme-dns01|acme-http01)
            _v5_internal_ca_present || return "${CHECK_MISSING}"
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
            _v5_apply_internal_ca
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
            # The internal CA is the anchor of every name here, public ones
            # included: there is no other issuer.
            _v5_apply_internal_ca
            warn "Certificates from this anchor are not publicly trusted."
            warn "  Import ${ns}/gentian-root-ca-tls ca.crt (written by B-08) into any"
            warn "  client that validates kernel hostnames from outside the cluster."
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
    # Only the issuers this installer labelled, the internal CA's
    # ServiceAccount and token Role, and a root CA Certificate left from
    # before the vault signed. A ClusterIssuer somebody else put on this
    # cluster is not ours to remove -- v4's step ended with a `delete
    # clusterissuer --all`, which on a shared cluster takes out everyone
    # else's anchors too. The CA itself stays in the vault: a reinstall gets
    # the same one back.
    local ns; ns="$(_v5_cert_manager_namespace)"
    kubectl delete clusterissuer -l app.kubernetes.io/managed-by=gentian-install \
        --ignore-not-found=true >/dev/null 2>&1 || true
    kubectl delete rolebinding,role,serviceaccount -n "${ns}" -l gentianos.io/issuer-role=internal-ca \
        --ignore-not-found=true >/dev/null 2>&1 || true
    kubectl delete externalsecret gentian-root-ca-tls -n "${ns}" \
        --ignore-not-found=true >/dev/null 2>&1 || true
    kubectl delete certificate gentian-root-ca -n "${ns}" \
        --ignore-not-found=true >/dev/null 2>&1 || true
}
