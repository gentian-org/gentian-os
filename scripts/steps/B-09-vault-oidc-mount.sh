#!/usr/bin/env bash
# step: B-09-vault-oidc-mount
# phase: secrets
# requires: B-08-seed-secrets
# provides: the oidc auth mount in OpenBao
# mutates: OpenBao auth mount oidc/

# The mount is enabled here and not by the Cluster composition, because this
# step is strictly more capable than the composition can be.
#
# provider-vault's jwt AuthBackend creates the mount AND writes its config in
# one resource, and it cannot adopt a mount it did not create. The provider
# also creates asynchronously and writes the result back to status, so when
# that write loses a conflict the id is never recorded, and every later
# reconcile tries to create again and gets 400 "path is already in use". A
# mount left behind by a partial create is therefore permanent: nothing in
# Crossplane recovers from it, and the whole XCluster stays not-Ready behind
# it. So the composition observes the mount and composes the roles against it,
# and something outside Crossplane makes it exist. That is this step.
#
# It runs before C-01 because C-01 is what applies the claim the composition
# reconciles, and those roles attach to `backend: oidc`. Without this step they
# compose against a mount that is not there, and OpenBao accepts no Keycloak
# login at all.
#
# The mount ONLY. Writing auth/oidc/config needs the Keycloak client secret,
# which does not exist until the realm does, and needs Keycloak to be serving
# its discovery document, which is several phases away. Held together in one
# step, the second half could never succeed on the pass that ran it.

_v5_oidc_ns() { ns_kernel secrets; }

# The address and token, resolved the way every other v5 vault step does.
# Steps cannot assume BAO_ADDR points anywhere: this one inherited
# http://127.0.0.1:8200 with nothing listening in v4 and failed on connection
# refused, which reads as OpenBao being down rather than as nobody having
# opened a tunnel.
_v5_oidc_bao() {
    local addr token
    addr="$(OPENBAO_NAMESPACE="$(_v5_oidc_ns)" gentian_service_addr openbao "$(_v5_oidc_ns)" 8200 https 2>/dev/null)" || return 1
    token="${BAO_TOKEN:-$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}" 2>/dev/null)}"
    [[ -n "${token}" ]] || return 1
    BAO_ADDR="${addr}"
    BAO_TOKEN="${token}"
    export BAO_ADDR BAO_TOKEN VAULT_SKIP_VERIFY=true BAO_SKIP_VERIFY=true
    return 0
}

# Is OIDC configured for this cluster at all?
#
# From the claim FILE first: this step runs before C-01, which is the only
# thing that applies that claim, so the object on the cluster is a step behind
# the file here and reading only the object would act on the previous run's
# configuration.
_v5_oidc_configured() {
    local claim_file url
    claim_file="${GENTIAN_DEPLOYMENTS_PATH:-}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}/kernel/claims/cluster.yaml"
    url="$(yq_get '.spec.oidc.discoveryUrl' "${claim_file}" 2>/dev/null || true)"
    if [[ -z "${url}" ]]; then
        url="$(kubectl get cluster.gentianos.io -n "$(ns_kernel provisioning)" \
            -o jsonpath='{.items[0].spec.oidc.discoveryUrl}' 2>/dev/null || true)"
    fi
    [[ -n "${url}" ]]
}

_v5_oidc_auth_list() {
    curl -sk --max-time 10 -H "X-Vault-Token: ${BAO_TOKEN}" "${BAO_ADDR}/v1/sys/auth"
}

check() {
    # No OIDC for this cluster: nothing to say about a mount not meant to be
    # here. Undefined rather than satisfied, so --status does not claim work
    # that was never asked for.
    _v5_oidc_configured || return "${CHECK_UNDEFINED}"
    # No token or no route is MISSING, not undefined.
    #
    # This said "cannot tell" and returned undefined, on the reasoning that
    # reporting missing would blame the cluster for a gap in this shell. The
    # driver skips an undefined step on the forward pass -- so a pass that
    # could not reach the vault at check time never enabled the mount, and
    # said "nothing to do here" while doing nothing. Every later step that
    # needs the mount then failed for reasons that name something else.
    #
    # Missing is the honest verdict: it makes apply() run, and apply() already
    # says exactly what is wrong and stops. A check that cannot verify a step
    # must never be the reason the step is skipped.
    _v5_oidc_bao || return "${CHECK_MISSING}"

    # The mount, and only the mount. Whether it is CONFIGURED is a later
    # step's verdict; asking it here would report this step unsatisfied for
    # the whole of a first install, on account of work it does not do.
    local body
    body="$(_v5_oidc_auth_list)" || return "${CHECK_MISSING}"
    # Permission denied is a token that cannot answer the question, which is
    # the same "cannot tell" as above and gets the same verdict.
    if grep -qi 'permission denied' <<<"${body}"; then
        return "${CHECK_MISSING}"
    fi
    jq -e '.data["oidc/"] // .["oidc/"]' >/dev/null 2>&1 <<<"${body}"
}

apply() {
    banner "OpenBao OIDC mount"

    if ! _v5_oidc_configured; then
        info "spec.oidc.discoveryUrl is unset; no OIDC mount to enable."
        return 0
    fi
    if ! _v5_oidc_bao; then
        error "Cannot reach OpenBao in $(_v5_oidc_ns), or no root token available."
        error "  Run B-03-vault-init first, or set BAO_TOKEN in this shell."
        return 1
    fi

    # Enabling is the only part that is not idempotent, so it is the only part
    # guarded. A second enable answers 400 "path is already in use", which is
    # the state the composition cannot recover from.
    local body
    body="$(_v5_oidc_auth_list)" || {
        error "Could not list OpenBao auth mounts."
        return 1
    }
    if jq -e '.data["oidc/"] // .["oidc/"]' >/dev/null 2>&1 <<<"${body}"; then
        info "Auth mount oidc/ already present."
        return 0
    fi

    info "Enabling auth mount oidc/..."
    local code
    code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 15 -X POST \
        -H "X-Vault-Token: ${BAO_TOKEN}" -H "Content-Type: application/json" \
        -d '{"type":"oidc"}' "${BAO_ADDR}/v1/sys/auth/oidc")"
    case "${code}" in
    20*)
        success "Auth mount oidc/ enabled. Its config follows once Keycloak is up."
        ;;
    *)
        error "Enabling the oidc auth mount returned HTTP ${code}."
        return 1
        ;;
    esac
}

destroy() {
    # Deliberately not disabling the mount.
    #
    # Disabling it revokes every token issued through it and deletes its
    # roles, which the composition then has to recreate. A teardown that
    # removes the cluster removes OpenBao with it; a partial teardown that
    # left the roles behind and took the mount away is exactly the
    # unrecoverable state this step exists to avoid.
    return 0
}
