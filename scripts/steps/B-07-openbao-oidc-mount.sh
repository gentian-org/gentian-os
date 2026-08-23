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
# _keycloak_deployed — is there a Keycloak in the services namespace at all?
#
# Presence, not readiness: this only decides whether an unreachable discovery
# document is "not yet" or "wrong". Matched by name rather than by the exact
# Service the keycloakx chart publishes, because the two callers that do need
# the precise Service (portal-login-bootstrap.sh) already try three spellings
# of it, and getting that wrong here would turn a deferral into a hard failure
# on a cluster where Keycloak is present under a name this did not predict.
_keycloak_deployed() {
    local ns
    ns="$(gentian_services_namespace 2>/dev/null || echo platform-kernel)"
    kubectl get svc -n "${ns}" -o name 2>/dev/null | grep -qi keycloak
}

_oidc_bao_addr() {
    [[ -n "${BAO_TOKEN:-}" ]] || return 1
    if BAO_ADDR="$(gentian_service_addr openbao openbao 8200 https)"; then
        export BAO_ADDR
        export VAULT_SKIP_VERIFY=true BAO_SKIP_VERIFY=true
        return 0
    fi
    return 1
}

# Values come from the claim FILE, not the live claim.
#
# This step runs before B-08, which is the only thing that applies that claim —
# the claims ApplicationSet deliberately excludes it. So the object on the
# cluster is always one step behind the file here, and reading it made this step
# act on the previous run's configuration: a corrected discoveryUrl in Git was
# invisible, and the same wrong URL was reported forever.
#
# The file is authored before either reader, which is the same reasoning
# load_deployments_cluster_settings gives for reading networkMode and nodeIp
# from it: one document, two readers, no second surface to keep in step.
#
# The live claim is the fallback, for a cluster whose checkout is not to hand.
_oidc_values() {
    local claim_file
    claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml"

    OIDC_DISCOVERY_URL="$(yq_get '.spec.oidc.discoveryUrl' "${claim_file}" 2>/dev/null || true)"
    OIDC_CLIENT_ID="$(yq_get '.spec.oidc.clientId' "${claim_file}" 2>/dev/null || true)"
    OIDC_SECRET_NS="$(yq_get '.spec.openbao.namespace' "${claim_file}" 2>/dev/null || true)"

    if [[ -z "${OIDC_DISCOVERY_URL}" ]]; then
        OIDC_DISCOVERY_URL="$(kubectl get cluster.gentianos.io -n crossplane-system \
            -o jsonpath='{.items[0].spec.oidc.discoveryUrl}' 2>/dev/null || true)"
        [[ -n "${OIDC_DISCOVERY_URL}" ]] &&
            info "  Using spec.oidc from the live claim; ${claim_file} was not readable."
    fi
    OIDC_CLIENT_ID="${OIDC_CLIENT_ID:-openbao}"
    OIDC_SECRET_NS="${OIDC_SECRET_NS:-openbao}"
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
    #
    # A refused read says nothing about the mount. A token OpenBao rejects — the
    # revoked bootstrap token from a completed install is the common one — got
    # its 403 swallowed here, the empty answer read as a mismatch, and apply()
    # was sent into the same 403 to fail loudly. Permission denied is a gap in
    # this shell, same as no token at all: cannot tell.
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
        info "spec.oidc.discoveryUrl is unset; no OIDC mount to configure."
        return 0
    fi

    if ! _oidc_bao_addr; then
        error "Cannot reach OpenBao on :8200, and no BAO_TOKEN in this shell."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        return 1
    fi

    # Enable is the only part that is not idempotent, so it is the only part
    # guarded. Configuring is a write that converges.
    #
    # Deliberately before the client-secret lookup, not after. The secret comes
    # from an ExternalSecret that lives in the Cluster XR composition, which
    # B-08 applies — and B-08 requires this step. Failing here when the secret
    # is absent therefore deadlocked every genuinely fresh install: B-07 could
    # not pass until B-08 had run, B-08 could not run until B-07 passed, and a
    # re-run reproduced it exactly because nothing in between created the
    # secret. Enabling the mount needs no secret, and the mount is precisely
    # what this step owes B-08 (see the header: the composed roles and
    # policies "only need the mount to exist first").
    if bao auth list -format=json 2>/dev/null | jq -e '."oidc/"' >/dev/null 2>&1; then
        info "Auth mount oidc/ already present."
    else
        info "Enabling auth mount oidc/..."
        gentian_run bao auth enable -path=oidc oidc || {
            error "Could not enable the oidc auth mount."
            return 1
        }
    fi

    local secret
    secret="$(kubectl get secret openbao-oidc-client -n "${OIDC_SECRET_NS}" \
        -o jsonpath='{.data.client-secret}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [[ -z "${secret}" ]]; then
        # Not an error: on a first install the ExternalSecret that materialises
        # this does not exist yet, and the mount above is all B-08 needs to get
        # far enough to create it. check() still reports this step unsatisfied
        # until the config is actually written, so the driver's end-of-run
        # report names it as outstanding and the next pass completes it.
        info "Secret ${OIDC_SECRET_NS}/openbao-oidc-client has no client-secret yet."
        info "  It is materialised by ESO from gentian-os/kernel/oidc/openbao,"
        info "  which the Cluster XR (B-08) creates. The mount is in place, so"
        info "  B-08 can proceed; re-run to write auth/oidc/config once it syncs."
        return 0
    fi

    # Keycloak has to exist before its discovery document can mean anything.
    #
    # It arrives at sync-wave 9 through the root ApplicationSet, which C-02
    # applies — several phases after this step. So on a first install the realm
    # is not serving yet, and an unreachable discovery URL says nothing about
    # whether that URL is correct. Failing here stopped the install at B-07 on
    # exactly the pass that would have gone on to bring Keycloak up.
    #
    # Deferring rather than failing, the same way the missing client secret
    # above does: check() still reports this step unsatisfied until
    # auth/oidc/config is readable, so the driver's end-of-run report keeps
    # naming it until a later pass — with Keycloak up — completes it.
    if ! _keycloak_deployed; then
        info "Keycloak is not deployed yet, so its discovery document cannot be"
        info "  checked. It arrives at sync-wave 9 via the root ApplicationSet"
        info "  (C-02), after this step. The mount is in place; re-run once"
        info "  Keycloak is serving to write auth/oidc/config."
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
    # a real fault — a wrong path or a broken realm — and still stops the run.
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
