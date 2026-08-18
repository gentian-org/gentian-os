#!/usr/bin/env bash
# step: B-07-openbao-oidc-mount
# phase: secrets
# requires: B-06-crossplane-secrets
# provides: the oidc auth mount, configured against Keycloak
# mutates: OpenBao auth mount oidc/ and its config

# The mount is owned here rather than by the Cluster composition, for the same
# reason A-06 owns the ClusterIssuers: this step is strictly more capable.
#
# provider-vault's jwt AuthBackend creates the mount AND writes its config in one
# resource, and it cannot adopt a mount it did not create. The provider also
# creates asynchronously and writes the result back to status, so when that write
# loses a conflict the id is never recorded — and every later reconcile tries to
# create again and answers 400 "path is already in use". A mount left by a
# partial create is therefore permanent: nothing in Crossplane can recover from
# it, and the whole XCluster stays not-Ready behind it.
#
# What Crossplane cannot express is "ensure a mount exists at this path and is
# configured". That is what this step does, and it converges from any starting
# state: mount present or absent, config right, wrong, or missing.
#
# The roles and policies stay in the composition. They reconcile correctly, they
# are the part that benefits from continuous reconciliation, and they only need
# the mount to exist first — which is why this step precedes B-08.
#
# Only the write is imperative. Every value comes from the claim or from
# OpenBao, exactly as A-06 reads certificates.issuerMode.

# The address, resolved the same way seed_secrets does: ClusterIP if it answers,
# a port-forward otherwise. Steps cannot assume BAO_ADDR is already pointing
# anywhere — this one inherited http://127.0.0.1:8200 with nothing listening and
# failed on connection refused, which reads as OpenBao being down rather than as
# nobody having opened the tunnel.
_oidc_bao_addr() {
    [[ -n "${BAO_TOKEN:-}" ]] || return 1
    if BAO_ADDR="$(gentian_service_addr openbao openbao 8200 https)"; then
        export BAO_ADDR
        export VAULT_SKIP_VERIFY=true BAO_SKIP_VERIFY=true
        return 0
    fi
    return 1
}

_oidc_values() {
    OIDC_DISCOVERY_URL="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.oidc.discoveryUrl}' 2>/dev/null || true)"
    OIDC_CLIENT_ID="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.oidc.clientId}' 2>/dev/null || true)"
    OIDC_CLIENT_ID="${OIDC_CLIENT_ID:-openbao}"
    local ns
    ns="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.openbao.namespace}' 2>/dev/null || true)"
    OIDC_SECRET_NS="${ns:-openbao}"
}

check() {
    _oidc_values
    # No OIDC configured for this cluster: nothing to say about a mount that is
    # not meant to exist here.
    [[ -n "${OIDC_DISCOVERY_URL}" ]] || return "${CHECK_UNDEFINED}"
    # No token, or no route to OpenBao, is "cannot tell" rather than "missing":
    # reporting missing would blame the cluster for a gap in this shell.
    _oidc_bao_addr || return "${CHECK_UNDEFINED}"

    # Configured means the discovery URL matches the claim, not merely that some
    # config exists — a mount pointed at the wrong realm authenticates nobody and
    # would otherwise read as satisfied.
    local have
    have="$(bao read -field=oidc_discovery_url auth/oidc/config 2>/dev/null || true)"
    [[ "${have}" == "${OIDC_DISCOVERY_URL}" ]]
}

apply() {
    _oidc_values
    if [[ -z "${OIDC_DISCOVERY_URL}" ]]; then
        info "spec.oidc.discoveryUrl is unset; no OIDC mount to configure."
        return 0
    fi

    if ! _oidc_bao_addr; then
        error "Cannot reach OpenBao on :8200, and no BAO_TOKEN in this shell."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        return 1
    fi

    local secret
    secret="$(kubectl get secret openbao-oidc-client -n "${OIDC_SECRET_NS}" \
        -o jsonpath='{.data.client-secret}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [[ -z "${secret}" ]]; then
        error "Secret ${OIDC_SECRET_NS}/openbao-oidc-client has no client-secret yet."
        error "  It is materialised by ESO from gentian-os/kernel/oidc/openbao."
        error "  Re-run once that ExternalSecret has synced."
        return 1
    fi

    # Enable is the only part that is not idempotent, so it is the only part
    # guarded. Configuring is a write that converges.
    if bao auth list -format=json 2>/dev/null | jq -e '."oidc/"' >/dev/null 2>&1; then
        info "Auth mount oidc/ already present."
    else
        info "Enabling auth mount oidc/..."
        gentian_run bao auth enable -path=oidc oidc || {
            error "Could not enable the oidc auth mount."
            return 1
        }
    fi

    # Check the discovery document before handing the URL to OpenBao. OpenBao's
    # own refusal is "error checking oidc discovery URL", which does not say
    # whether the name failed to resolve, the TLS was rejected, or the realm
    # simply is not at that path — and the last is the common one, because
    # Keycloak serves either /realms/<realm> or /auth/realms/<realm> depending on
    # how it was deployed.
    local well_known="${OIDC_DISCOVERY_URL%/}/.well-known/openid-configuration"
    if ! run_validator oidc-discovery "${well_known}" >/dev/null 2>&1; then
        error "The OIDC discovery document is not readable at:"
        error "  ${well_known}"
        error "  OpenBao must fetch this to accept the config, so it is checked first."
        case "${OIDC_DISCOVERY_URL}" in
            */auth/realms/*) : ;;
            */realms/*)
                error "  Some Keycloak deployments serve the realm under /auth. Try:"
                error "    ${OIDC_DISCOVERY_URL/\/realms\//\/auth\/realms\/}" ;;
        esac
        error "  Fix spec.oidc.discoveryUrl on the Cluster claim and re-run."
        return 1
    fi

    info "Configuring auth/oidc/config against ${OIDC_DISCOVERY_URL}..."
    gentian_run bao write auth/oidc/config \
        oidc_discovery_url="${OIDC_DISCOVERY_URL}" \
        oidc_client_id="${OIDC_CLIENT_ID}" \
        oidc_client_secret="${secret}" \
        default_role="cluster-admin" || {
        error "Could not write auth/oidc/config."
        return 1
    }
    success "OIDC auth mount configured (client ${OIDC_CLIENT_ID})."
}

destroy() {
    # Removing the mount takes every role and policy under it with it, which is
    # what an uninstall wants and what a re-run must never do.
    _oidc_bao_addr || return 0
    if bao auth list -format=json 2>/dev/null | jq -e '."oidc/"' >/dev/null 2>&1; then
        gentian_run bao auth disable oidc || true
    fi
}
