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
# Refuses to run when there is no OIDC write path configured, because revoking
# the only way in leaves a cluster nobody can supply a credential to. That is
# the one case where NOT revoking is correct, and it is reported rather than
# silently skipped.

_oidc_configured() {
    kubectl get cluster.gentianos.io -n crossplane-system \
        -o jsonpath='{.items[0].spec.oidc.discoveryUrl}' 2>/dev/null | grep -q .
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
    _oidc_configured || return "${CHECK_UNDEFINED}"

    ! bao token lookup >/dev/null 2>&1
}

apply() {
    if ! _oidc_configured; then
        warn "No OIDC write path configured (spec.oidc.discoveryUrl is unset)."
        warn "  Refusing to revoke the bootstrap token: it is currently the ONLY"
        warn "  way to write a credential, and revoking it would leave this"
        warn "  cluster unable to accept one."
        warn "  Configure spec.oidc on the Cluster claim, then re-run:"
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
