#!/usr/bin/env bash
# step: D-06-openbao-oidc-config
# phase: applications
# requires: D-05-portal-login
# provides: auth/oidc/config, so OpenBao accepts Keycloak logins
# mutates: OpenBao auth/oidc/config

# The second half of B-07-openbao-oidc-mount, placed where its dependencies
# actually are.
#
# B-07 enables the oidc mount, and must: the composed AuthBackendRoles and the
# observe-only AuthBackend in the Cluster composition attach to it, so it has
# to exist before B-08. Writing the mount's CONFIG needs two things that do not
# exist anywhere near that point:
#
#   the Keycloak client secret — ESO materialises it from a KV path the Cluster
#   XR creates, so it cannot precede B-08;
#
#   Keycloak itself, serving its discovery document — it arrives at sync-wave 9
#   through the root ApplicationSet, which C-02 applies, and D-05 is the first
#   step that waits for it to answer.
#
# Held in B-07 those two could never be satisfied on the pass that ran it. The
# install stopped there on every fresh cluster: first on the absent client
# secret, then, once that was deferred, on the unreachable discovery URL. The
# step was not wrong, its slot was — it was ordered by subject (OpenBao, so
# phase B) rather than by dependency.
#
# Splitting it is what makes a single pass possible. Nothing here defers as its
# normal mode any more; the deferrals below are a safety net for a cluster
# whose Keycloak is late, not the mechanism by which an install completes.

_oidc_bao_addr() {
    [[ -n "${BAO_TOKEN:-}" ]] || return 1
    if BAO_ADDR="$(gentian_service_addr openbao openbao 8200 https)"; then
        export BAO_ADDR
        export VAULT_SKIP_VERIFY=true BAO_SKIP_VERIFY=true
        return 0
    fi
    return 1
}

# The live claim first, unlike B-07 which reads the file.
#
# B-07 runs before B-08, the only thing that applies the claim, so the object
# on the cluster is always one revision behind the file there. By this step
# B-08 has applied it, so the object IS the applied configuration — and reading
# it means this step configures the realm the cluster actually composed against
# rather than whatever the checkout happens to say now. The file is the
# fallback, for a cluster whose claim was never applied.
_oidc_values() {
    OIDC_DISCOVERY_URL="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.oidc.discoveryUrl}' 2>/dev/null || true)"
    OIDC_CLIENT_ID="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.oidc.clientId}' 2>/dev/null || true)"
    OIDC_SECRET_NS="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.openbao.namespace}' 2>/dev/null || true)"

    if [[ -z "${OIDC_DISCOVERY_URL}" ]]; then
        local claim_file
        claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml"
        OIDC_DISCOVERY_URL="$(yq_get '.spec.oidc.discoveryUrl' "${claim_file}" 2>/dev/null || true)"
        OIDC_CLIENT_ID="$(yq_get '.spec.oidc.clientId' "${claim_file}" 2>/dev/null || true)"
        OIDC_SECRET_NS="$(yq_get '.spec.openbao.namespace' "${claim_file}" 2>/dev/null || true)"
        [[ -n "${OIDC_DISCOVERY_URL}" ]] &&
            info "  Using spec.oidc from ${claim_file}; no applied claim was readable."
    fi
    OIDC_CLIENT_ID="${OIDC_CLIENT_ID:-openbao}"
    OIDC_SECRET_NS="${OIDC_SECRET_NS:-openbao}"
}

# Presence, not readiness: this only decides whether an unreachable discovery
# document is "not yet" or "wrong". Matched by name rather than by the exact
# Service the keycloakx chart publishes, because the caller that does need the
# precise Service (portal-login-bootstrap.sh) already tries three spellings of
# it, and predicting it wrong here would turn a deferral into a hard failure on
# a cluster where Keycloak is present under an unexpected name.
_keycloak_deployed() {
    local ns
    ns="$(gentian_services_namespace 2>/dev/null || echo platform-kernel)"
    kubectl get svc -n "${ns}" -o name 2>/dev/null | grep -qi keycloak
}

check() {
    _oidc_values
    # No OIDC configured for this cluster: nothing to configure, and nothing to
    # report about a mount that is not meant to exist here.
    [[ -n "${OIDC_DISCOVERY_URL}" ]] || return "${CHECK_UNDEFINED}"
    # No token, or no route to OpenBao, is "cannot tell" rather than "missing":
    # reporting missing would blame the cluster for a gap in this shell.
    _oidc_bao_addr || return "${CHECK_UNDEFINED}"

    # Configured means the discovery URL matches the claim, not merely that some
    # config exists — a mount pointed at the wrong realm authenticates nobody and
    # would otherwise read as satisfied.
    #
    # A refused read says nothing about the config. A token OpenBao rejects —
    # the revoked bootstrap token from a completed install is the common one —
    # got its 403 swallowed here, the empty answer read as a mismatch, and
    # apply() was sent into the same 403 to fail loudly. Permission denied is a
    # gap in this shell, same as no token at all: cannot tell.
    local have rc=0
    have="$(bao read -field=oidc_discovery_url auth/oidc/config 2>&1)" || rc=$?
    if [[ ${rc} -ne 0 ]]; then
        grep -qi 'permission denied' <<<"${have}" && return "${CHECK_UNDEFINED}"
        return "${CHECK_MISSING}"
    fi
    [[ "${have}" == "${OIDC_DISCOVERY_URL}" ]]
}

apply() {
    _oidc_values
    if [[ -z "${OIDC_DISCOVERY_URL}" ]]; then
        info "spec.oidc.discoveryUrl is unset; no OIDC config to write."
        return 0
    fi

    if ! _oidc_bao_addr; then
        error "Cannot reach OpenBao on :8200, and no BAO_TOKEN in this shell."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        return 1
    fi

    # B-07's mount has to be there. It is a required step, so absent here means
    # something removed it after the fact — say which step owns it rather than
    # letting `bao write` answer with a bare 404 on an unrelated-looking path.
    if ! bao auth list -format=json 2>/dev/null | jq -e '."oidc/"' >/dev/null 2>&1; then
        error "There is no oidc/ auth mount to configure."
        error "  B-07-openbao-oidc-mount enables it. Re-run that step first:"
        error "    ./install.sh --step B-07"
        return 1
    fi

    local secret
    secret="$(kubectl get secret openbao-oidc-client -n "${OIDC_SECRET_NS}" \
        -o jsonpath='{.data.client-secret}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [[ -z "${secret}" ]]; then
        # Safety net, not the normal path: B-08 created the ExternalSecret long
        # before this step, so by now ESO has almost always synced it.
        info "Secret ${OIDC_SECRET_NS}/openbao-oidc-client has no client-secret yet."
        info "  ESO materialises it from gentian-os/kernel/oidc/openbao. It should"
        info "  already exist by this step; re-run once it has synced."
        return 0
    fi

    if ! _keycloak_deployed; then
        # Also a safety net. D-05 has run by now and talks to Keycloak, so a
        # missing Keycloak here means it was removed or never came up.
        info "Keycloak is not deployed, so its discovery document cannot be checked."
        info "  It arrives at sync-wave 9 via the root ApplicationSet (C-02)."
        info "  Re-run once it is serving to write auth/oidc/config."
        return 0
    fi

    # Check the discovery document before handing the URL to OpenBao. OpenBao's
    # own refusal is "error checking oidc discovery URL", which does not say
    # whether the name failed to resolve, the TLS was rejected, or the realm
    # simply is not at that path — and the last is the common one, because
    # Keycloak serves either /realms/<realm> or /auth/realms/<realm> depending on
    # how it was deployed.
    #
    # Reached only when Keycloak IS deployed, so an unreadable document here is
    # a real fault — a wrong path or a broken realm — and stops the run.
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

# No destroy(): the config lives inside the oidc/ mount, and disabling that
# mount is B-07-openbao-oidc-mount's destroy(), which takes the config, the
# roles and the policies under it in one go. Deleting the config separately
# here would only add a second writer to an object one step already owns.
