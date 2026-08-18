#!/usr/bin/env bash
# step: E-03-revoke-bootstrap-token
# phase: handover
# requires: E-02-tenant-reconcile
# provides: an installer whose OpenBao token no longer works
# mutates: revokes BAO_TOKEN; removes the openbao-init Secret's root token

# The last step, and the one that closes the bootstrap exception.
#
# §6 says the write path is human-identified forever, with bootstrap as the
# single exception because there is no Keycloak yet. That exception only ends if
# something ends it: a root token that outlives the install is a credential with
# every capability, no expiry, and no name attached in the audit device. §7 asks
# for this as a scripted step rather than a runbook note precisely because a
# runbook note is a step that does not happen.
#
# Refuses to run until somebody has ACTUALLY LOGGED IN, because revoking the
# only way in leaves a cluster nobody can supply a credential to. That is the
# one case where NOT revoking is correct, and it is reported rather than
# silently skipped.
#
# Two earlier versions of this guard were both wrong in the same direction.
#
# The first checked that spec.oidc.discoveryUrl was set — that a URL had been
# DECLARED, not that anything answered at it. Declaring it is also what makes
# the OIDC roles render at all, so the intended sequence (set the URL, let the
# identity objects reconcile, revoke) passed the guard at the first step rather
# than the last.
#
# The second read the cluster the way every other check() does and verified the
# whole chain: discovery URL set, Keycloak client Ready, client Secret present,
# auth backend configured, role exists. Every link absent by default and present
# only when something created it — and every one of them true while no token
# opens anything. The role binds a group claim, and if Keycloak emits
# "superadmin" where the role expects "/gentian:platform:superadmin", the parts
# are all there and the login is refused. Revoking on that evidence is changing
# the locks and posting the old key through the letterbox without trying the new
# one; the recovery is re-initialising OpenBao.
#
# A shell script cannot perform an interactive login, so it cannot produce the
# proof. But it does not have to: the credential manager performs exactly this
# exchange on every request it serves, and records the first cluster-admin
# success in the gentian-handover ConfigMap. That record IS the proof, made by
# the component that was going to do it anyway, and this step reads it.
#
# The chain checks are kept — not as the guard, but as the diagnosis. When the
# proof is absent they say which link to fix, which "nobody has logged in yet"
# on its own does not.

# _handover_proven — has anyone actually authenticated and been given the
# cluster-admin policy?
#
# Written by the credential manager on the first successful exchange. Read from
# Kubernetes, not from OpenBao, so --status can answer it with no token — which
# is the whole reason it is a ConfigMap and not a fact only OpenBao knows.
_handover_proven() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    [[ "$(kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.writePathProven}' 2>/dev/null || true)" == "true" ]]
}

_handover_proof_detail() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.provenBy} at {.data.provenAt}' 2>/dev/null || true
}

# _oidc_write_path_ready — every link between a human and an OpenBao token.
#
# Prints what is missing, because "refused to revoke" is only actionable if it
# says which part to fix.
_oidc_write_path_ready() {
    local missing=()

    local discovery
    discovery="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.oidc.discoveryUrl}' 2>/dev/null || true)"
    [[ -n "${discovery}" ]] || missing+=("spec.oidc.discoveryUrl is unset on the Cluster claim")

    # The Keycloak client. Read as a Crossplane resource rather than by asking
    # Keycloak, because Ready=True is that provider reporting the object exists
    # at the far end — which is the fact this needs, and needs no admin
    # credential to establish.
    local client_ready
    client_ready="$(kubectl get client.openidclient.keycloak.crossplane.io \
        -l gentianos.io/purpose=openbao-oidc \
        -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    [[ "${client_ready}" == "True" ]] || missing+=("the OpenBao OIDC client in Keycloak is not Ready")

    # The Secret the auth backend reads its client secret from. Present only if
    # the ExternalSecret behind it synced.
    local ns
    ns="$(kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.openbao.namespace}' 2>/dev/null || true)"
    ns="${ns:-openbao}"
    kubectl get secret openbao-oidc-client -n "${ns}" >/dev/null 2>&1 ||
        missing+=("Secret ${ns}/openbao-oidc-client does not exist")

    # OpenBao's own view: the backend mounted, and a role to log in against. A
    # backend with no role accepts no one.
    if [[ -n "${BAO_TOKEN:-}" ]]; then
        bao read -field=oidc_client_id auth/oidc/config >/dev/null 2>&1 ||
            missing+=("OpenBao's oidc auth backend is not configured")
        bao read auth/oidc/role/cluster-admin >/dev/null 2>&1 ||
            missing+=("OpenBao has no cluster-admin OIDC role")
    fi

    if [[ ${#missing[@]} -gt 0 ]]; then
        _OIDC_MISSING="$(printf '%s\n' "${missing[@]}")"
        return 1
    fi
    return 0
}

check() {
    # Satisfied when the bootstrap token no longer authenticates. A token that
    # still works is unfinished business, not a completed step.
    #
    # No token in this shell is not the same as a revoked one: --status runs
    # without loading credentials, and reporting "revoked" there would announce
    # the install's last safety step as done on a cluster that never installed.
    [[ -n "${BAO_TOKEN:-}" ]] || return "${CHECK_UNDEFINED}"

    # Revocation needs somewhere else to write credentials from afterwards, and
    # apply() refuses without it — correctly, since the bootstrap token is
    # otherwise the only write path. Reporting missing then is a step that
    # applies on every run and declines every time, which reads as an unfinished
    # install rather than a cluster whose administrator has not signed in yet.
    _handover_proven || return "${CHECK_UNDEFINED}"

    ! bao token lookup >/dev/null 2>&1
}

apply() {
    if ! _handover_proven; then
        warn "Nobody has signed in to this cluster as an administrator yet."
        warn "  Refusing to revoke the bootstrap token: it is currently the ONLY"
        warn "  way to write a credential, and revoking it before the sign-in"
        warn "  path is known to work would leave this cluster unable to accept"
        warn "  one — recovering means re-initialising OpenBao."
        warn ""
        warn "  Sign in to the portal as the cluster administrator. The first"
        warn "  successful sign-in records the proof, and then:"
        warn "    ./install.sh --only E-03"
        warn ""
        # The chain is the diagnosis, not the guard. "Nobody has logged in" is
        # true whether the operator simply has not got to it or the login is
        # broken, and only the second needs fixing before they try.
        if ! _oidc_write_path_ready; then
            warn "  A sign-in would not succeed yet — these are missing:"
            while IFS= read -r _reason; do
                [[ -n "${_reason}" ]] && warn "    - ${_reason}"
            done <<< "${_OIDC_MISSING:-}"
        else
            warn "  Every part of the sign-in path is in place; it has just not"
            warn "  been used. That is the one thing this installer cannot do"
            warn "  for you — the exchange needs a human at a browser."
        fi
        return 0
    fi

    info "Administrator sign-in confirmed ($(_handover_proof_detail))."

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        info "No bootstrap token in this shell; nothing to revoke."
        return 0
    fi

    info "Revoking the installer's bootstrap token..."
    # self-revoke rather than revoking by accessor: it needs no other
    # credential, and it works even if this token's own lookup rights were
    # narrowed.
    if bao token revoke -self >/dev/null 2>&1; then
        success "Bootstrap token revoked. Day-2 writes go through OIDC."
    else
        warn "Could not revoke the bootstrap token — revoke it by hand."
    fi
    unset BAO_TOKEN

    # The init Secret still holds the root token and the unseal keys. The root
    # token is now dead, but the Secret reads as a live credential to anyone who
    # finds it, so strip it and leave the unseal material alone.
    if kubectl get secret openbao-init -n openbao >/dev/null 2>&1; then
        gentian_run kubectl patch secret openbao-init -n openbao \
            --type=json -p '[{"op":"remove","path":"/data/root_token"}]' 2>/dev/null ||
            info "openbao-init carries no root_token key; nothing to strip."
    fi

    # Recorded beside the proof, in the same object, because the two questions
    # an operator asks about a cluster are "can my admin write" and "is the
    # installer's key gone" — and a cluster that answers yes to the first and no
    # to the second is unfinished rather than broken.
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    gentian_run kubectl patch configmap gentian-handover -n "${ns}" --type=merge \
        -p "{\"data\":{\"bootstrapCredentialRevoked\":\"true\",\"revokedAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}" \
        2>/dev/null || warn "Could not record the revocation in ${ns}/gentian-handover."
}

# No destroy(): a revoked token cannot be un-revoked, and teardown removes
# OpenBao's storage anyway.
