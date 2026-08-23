#!/usr/bin/env bash
# step: B-07-openbao-oidc-mount
# phase: secrets
# requires: B-06-crossplane-secrets
# provides: the oidc auth mount
# mutates: OpenBao auth mount oidc/

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
# The roles and policies stay in the composition. They reconcile correctly, they
# are the part that benefits from continuous reconciliation, and they only need
# the mount to exist first — which is why this step precedes B-08.
#
# The mount ONLY. Writing auth/oidc/config is D-08-openbao-oidc-config, because
# it cannot be done from here: the config needs the Keycloak client secret,
# which ESO materialises from a KV path B-08 creates, and it needs Keycloak
# itself to be serving its discovery document, which arrives at sync-wave 9
# through the root ApplicationSet that C-02 applies. Both are phases this step
# precedes. Held together in one step, its second half could never succeed on
# the pass that ran it, and the install stopped here on every fresh cluster —
# first for the absent secret, then for the unreachable discovery URL. Split,
# each half sits where its dependencies already are.

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

# Is OIDC configured for this cluster at all?
#
# From the claim FILE, not the live claim. This step runs before B-08, which is
# the only thing that applies that claim — the claims ApplicationSet
# deliberately excludes it. So the object on the cluster is always one step
# behind the file here, and reading it made this step act on the previous run's
# configuration.
#
# Only whether a discovery URL exists is needed now; its value is D-08's
# business, and D-08 reads the live claim because by then B-08 has applied it.
_oidc_configured() {
    local claim_file url
    claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml"
    url="$(yq_get '.spec.oidc.discoveryUrl' "${claim_file}" 2>/dev/null || true)"
    if [[ -z "${url}" ]]; then
        url="$(kubectl get cluster.gentianos.io -n crossplane-system \
            -o jsonpath='{.items[0].spec.oidc.discoveryUrl}' 2>/dev/null || true)"
    fi
    [[ -n "${url}" ]]
}

check() {
    # No OIDC configured for this cluster: nothing to say about a mount that is
    # not meant to exist here.
    _oidc_configured || return "${CHECK_UNDEFINED}"
    # No token, or no route to OpenBao, is "cannot tell" rather than "missing":
    # reporting missing would blame the cluster for a gap in this shell.
    _oidc_bao_addr || return "${CHECK_UNDEFINED}"

    # The mount, and only the mount. Whether it is CONFIGURED is D-08's verdict
    # to give; asking it here would report this step unsatisfied for the whole
    # of a first install, on account of work it does not do.
    #
    # A token OpenBao rejects — the revoked bootstrap token from a completed
    # install is the common one — is a gap in this shell, not a missing mount.
    local out rc=0
    out="$(bao auth list -format=json 2>&1)" || rc=$?
    if [[ ${rc} -ne 0 ]]; then
        grep -qi 'permission denied' <<<"${out}" && return "${CHECK_UNDEFINED}"
        return "${CHECK_MISSING}"
    fi
    jq -e '."oidc/"' >/dev/null 2>&1 <<<"${out}"
}

apply() {
    if ! _oidc_configured; then
        info "spec.oidc.discoveryUrl is unset; no OIDC mount to enable."
        return 0
    fi

    if ! _oidc_bao_addr; then
        error "Cannot reach OpenBao on :8200, and no BAO_TOKEN in this shell."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        return 1
    fi

    # Enable is the only part that is not idempotent, so it is the only part
    # guarded.
    if bao auth list -format=json 2>/dev/null | jq -e '."oidc/"' >/dev/null 2>&1; then
        info "Auth mount oidc/ already present."
        return 0
    fi

    info "Enabling auth mount oidc/..."
    gentian_run bao auth enable -path=oidc oidc || {
        error "Could not enable the oidc auth mount."
        return 1
    }
    success "Auth mount oidc/ enabled. D-08 configures it once Keycloak is up."
}

destroy() {
    # Removing the mount takes every role and policy under it with it — and the
    # config D-08 wrote, which is why D-08 has no destroy() of its own. That is
    # what an uninstall wants and what a re-run must never do.
    _oidc_bao_addr || return 0
    if bao auth list -format=json 2>/dev/null | jq -e '."oidc/"' >/dev/null 2>&1; then
        gentian_run bao auth disable oidc || true
    fi
}
