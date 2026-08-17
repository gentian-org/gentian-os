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
# Refuses to run when there is no WORKING OIDC write path, because revoking the
# only way in leaves a cluster nobody can supply a credential to. That is the
# one case where NOT revoking is correct, and it is reported rather than
# silently skipped.
#
# This used to check that spec.oidc.discoveryUrl was set — that a URL had been
# DECLARED, not that anything answered at it. Declaring it is also what makes
# the OIDC roles render at all, so the intended sequence (set the URL, let the
# identity objects reconcile, revoke) passed the guard at the first step rather
# than the last. A cluster whose Keycloak client did not exist would revoke its
# only write path and need OpenBao re-initialising to recover.
#
# So the guard now reads the cluster the way every other check() does, and
# checks the parts a human login actually needs. It cannot perform an
# interactive login, so it verifies the chain rather than the outcome — each
# link is a thing that is absent by default and present only when something
# created it.

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
    # install rather than a cluster that has not configured OIDC yet.
    _oidc_write_path_ready || return "${CHECK_UNDEFINED}"

    ! bao token lookup >/dev/null 2>&1
}

apply() {
    if ! _oidc_write_path_ready; then
        warn "The OIDC write path is not usable yet:"
        while IFS= read -r _reason; do
            [[ -n "${_reason}" ]] && warn "    - ${_reason}"
        done <<< "${_OIDC_MISSING:-}"
        warn "  Refusing to revoke the bootstrap token: it is currently the ONLY"
        warn "  way to write a credential, and revoking it would leave this"
        warn "  cluster unable to accept one — recovering means re-initialising"
        warn "  OpenBao."
        warn "  Fix the above, then re-run:"
        warn "    ./install.sh --only E-03"
        return 0
    fi

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
}

# No destroy(): a revoked token cannot be un-revoked, and teardown removes
# OpenBao's storage anyway.
