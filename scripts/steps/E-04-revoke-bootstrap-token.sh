#!/usr/bin/env bash
# step: E-04-revoke-bootstrap-token
# phase: handover
# requires: E-03-recovery-kit
# provides: an installer whose OpenBao token no longer works
# mutates: revokes BAO_TOKEN; deletes the local openbao-init file

# The last step, and the one that closes the bootstrap exception.
#
# §6 says the write path is human-identified forever, with bootstrap as the
# single exception because there is no Keycloak yet. That exception only ends if
# something ends it: a root token that outlives the install is a credential with
# every capability, no expiry, and no name attached in the audit device. §7 asks
# for this as a scripted step rather than a runbook note precisely because a
# runbook note is a step that does not happen.
#
# Refuses to run until somebody has ACTUALLY LOGGED IN, AND a recovery kit
# has been exported, because revoking with either missing leaves a cluster
# nobody can supply a credential to — an unproven login is one dead end, a
# kit that does not exist is the other, and losing either after this token is
# gone means re-initialising OpenBao. That is the one case where NOT
# revoking is correct, and it is reported rather than silently skipped.
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

# _kit_exported — has --export-recovery-kit actually written one?
#
# The second precondition, alongside _handover_proven. Revoking the bootstrap
# token is exactly the moment this cluster's only other way in becomes
# whatever a recovery kit holds — if OIDC turns out to be broken later, the
# kit is the recovery, not another look at this token. Written by
# recovery.sh's _record_kit_export_proof; read the same way _handover_proven
# reads its own fact, from Kubernetes rather than from OpenBao, so --status
# can answer it with no token.
_kit_exported() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    [[ "$(kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.recoveryKitExported}' 2>/dev/null || true)" == "true" ]]
}

_kit_exported_detail() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.recoveryKitExportedAt}' 2>/dev/null || true
}

# _bootstrap_token — the token this step exists to revoke.
#
# B-04 exports BAO_TOKEN, but this step is the one an operator runs on its own:
# the summary ends every incomplete install with `./install.sh --only E-04`,
# and a scoped run never reaches B-04. So BAO_TOKEN was unset, check() returned
# undefined, the step skipped, and the summary printed the same instruction
# again — telling the operator to run the command they had just run, forever.
#
# The token is in the init file, which is exactly where B-04 put it and where
# bootstrap_openbao_for_crossplane already reads it from on the same kind of
# re-run. Nothing new is trusted: this step deletes that file moments later.
_bootstrap_token() {
    [[ -n "${BAO_TOKEN:-}" ]] && { echo "${BAO_TOKEN}"; return 0; }
    local f="${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}"
    [[ -r "${f}" ]] || return 1
    local t; t="$(jq -r '.root_token // empty' "${f}" 2>/dev/null || true)"
    [[ -n "${t}" ]] || return 1
    echo "${t}"
}

# _bao_reachable — point this shell at OpenBao, or say we cannot ask.
#
# Steps cannot assume BAO_ADDR is set. With none, the bao CLI falls back to
# https://127.0.0.1:8200, which nothing is listening on — and every command
# fails with connection refused. Resolved the way B-07 does: the ClusterIP if it
# answers, a port-forward otherwise.
_bao_reachable() {
    if [[ -z "${BAO_TOKEN:-}" ]]; then
        BAO_TOKEN="$(_bootstrap_token)" || return 1
        export BAO_TOKEN
    fi
    if BAO_ADDR="$(gentian_service_addr openbao openbao 8200 https)"; then
        export BAO_ADDR
        export VAULT_SKIP_VERIFY=true BAO_SKIP_VERIFY=true
        return 0
    fi
    return 1
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
    # No token ANYWHERE is not the same as a revoked one: --status runs without
    # loading credentials, and reporting "revoked" there would announce the
    # install's last safety step as done on a cluster that never installed.
    # The init file counts as somewhere — see _bootstrap_token, without which
    # `--only E-04` could never do anything at all.
    _bootstrap_token >/dev/null || return "${CHECK_UNDEFINED}"

    # A kit is E-03's job and a hard precondition. Without one there is nothing
    # for this step to do but say so, and undefined keeps it out of the
    # end-of-run report on a cluster where the operator has not got there yet.
    _kit_exported || return "${CHECK_UNDEFINED}"

    # The sign-in is NOT tested here, deliberately.
    #
    # apply() waits for it — that is what makes handover one command rather
    # than an instruction to come back later — so a cluster where nobody has
    # signed in yet is a step with work to do, not a step that cannot say. It
    # reported undefined once, which meant the driver skipped the wait
    # entirely and the install ended by asking the operator to run the step
    # that had just declined to run.

    # Reachability BEFORE the question, and its own verdict when absent.
    #
    # This step reports satisfied when the token no longer authenticates, and
    # it asked by negating `bao token lookup` — so a shell that could not reach
    # OpenBao at all got connection refused, the negation turned that into
    # true, and the step reported the bootstrap token revoked when it was still
    # live. The driver then skipped the one action that closes the install, and
    # the run ended in "Bootstrap complete".
    #
    # Nothing about a failed connection says anything about a token. That is
    # CHECK_UNDEFINED, and it is the distinction this whole step exists to
    # insist on: observe the fact, never infer it from a proxy.
    _bao_reachable || return "${CHECK_UNDEFINED}"

    ! bao token lookup >/dev/null 2>&1
}

# _wait_for_sign_in — hold the install open while a human signs in.
#
# This is the whole reason handover used to take three commands. Everything
# else here is automatic; the exchange is not, because only a person at a
# browser can perform it. Waiting for it turns "run this, then that, then this
# again" into one run that pauses and tells you what it is waiting for.
#
# Bounded and interruptible on purpose: Ctrl-C leaves a cluster that is
# installed and un-revoked, which is exactly the state a later
# `./install.sh --only E-04` finishes from. A timeout is the same state.
_wait_for_sign_in() {
    local timeout="${GENTIAN_HANDOVER_WAIT_SECS:-1800}"
    local url="https://portal.${KERNEL_DOMAIN:-<kernel-domain>}/login"
    local waited=0 interval=10

    echo ""
    warn "╔══════════════════════════════════════════════════════════════════╗"
    warn "║  WAITING FOR YOU — two things left                               ║"
    warn "╚══════════════════════════════════════════════════════════════════╝"
    echo ""

    # Both instructions here, in the colour the rest of the installer uses for
    # things that matter.
    #
    # The kit warning used to be printed by E-03, several minutes and a few
    # hundred lines earlier, and had scrolled off by the time the install
    # stopped to wait. The one screen an operator is actually looking at — the
    # one that is not moving — has to carry everything they are being asked to
    # do. The path comes from the handover record rather than being recomputed,
    # so this names the file E-03 actually wrote.
    local kit_path
    kit_path="$(kubectl get configmap gentian-handover \
        -n "${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}" \
        -o jsonpath='{.data.recoveryKitPath}' 2>/dev/null || true)"

    warn "  1. MOVE THE RECOVERY KIT SOMEWHERE SAFE"
    if [[ -n "${kit_path}" ]]; then
        warn "       ${kit_path}"
    else
        warn "       the gentian-recovery-kit-*.age file beside this checkout"
    fi
    warn "     A password manager, a sealed vault, offline media. Without it"
    warn "     this cluster cannot be rebuilt as itself, and there is no way"
    warn "     back into OpenBao if its login path ever breaks."
    echo ""
    warn "  2. SIGN IN as the cluster administrator"
    echo ""

    # The credentials, here, rather than a pointer to them.
    #
    # This said "credentials are in the summary above" — and the summary is
    # printed AFTER the steps, so at this moment there is no summary above.
    # D-05 printed them, several hundred lines and many minutes earlier. An
    # instruction to sign in that does not say what to sign in with sends the
    # operator scrolling, which is the opposite of what a step that has stopped
    # to wait for them should do.
    #
    # print_portal_login_summary is where this already lives and derives the
    # password the same way the account was created with. Its library is not in
    # load.sh's set, so source it the way print_summary_cp does.
    #
    # Its output is captured rather than printed directly, because it returns
    # silently when KERNEL_DOMAIN is unset or the password cannot be derived —
    # reasonable for a summary that has other things to say, useless for a
    # prompt whose entire purpose is to tell the operator what to sign in with.
    # A step that has stopped to wait must never go quiet.
    local creds=""
    if [[ -f "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh" ]]; then
        # shellcheck source=scripts/lib/portal-login-bootstrap.sh
        source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
        creds="$(print_portal_login_summary 2>/dev/null || true)"
    fi
    if [[ -n "${creds}" ]]; then
        printf '%s\n' "${creds}"
    else
        warn "  ${url}"
        warn "  User: administrator@${KERNEL_DOMAIN:-<kernel-domain>}"
        warn "  Password: derived — print it with ./install.sh --verify-only"
    fi
    echo ""
    info "  Signing in proves someone other than the installer can write"
    info "  credentials. Until it happens the installer's own credential has to"
    info "  stay live, so this is the last thing between here and a finished"
    info "  cluster."
    echo ""
    info "  Waiting up to $(( timeout / 60 )) minutes. Ctrl-C is safe — the"
    info "  cluster stays as it is and ./install.sh picks up from here."
    echo ""

    while (( waited < timeout )); do
        if _handover_proven; then
            echo ""
            success "Sign-in recorded ($(_handover_proof_detail))."
            return 0
        fi
        sleep "${interval}"
        waited=$(( waited + interval ))
        (( waited % 60 == 0 )) && info "  still waiting ($(( waited / 60 ))m of $(( timeout / 60 ))m)..."
    done
    return 1
}

apply() {
    # Both preconditions collected before either is reported, not checked
    # sequentially — an operator missing both should see both gaps on the
    # first run, not fix one, re-run, and only then learn about the second.
    local proven=1 kit=1
    _handover_proven && proven=0
    _kit_exported && kit=0

    # Wait for the sign-in rather than refusing over it.
    #
    # Only when a kit exists: without one, revoking is wrong no matter who
    # signs in, so there is nothing to wait for. And only when someone is there
    # to sign in — an unattended run has no browser, and blocking a pipeline
    # for half an hour to reach the same refusal helps nobody.
    if (( proven != 0 && kit == 0 )) \
       && [[ "${GENTIAN_NONINTERACTIVE:-0}" != "1" && "${GENTIAN_HANDOVER_NO_WAIT:-0}" != "1" ]]; then
        if _wait_for_sign_in; then
            proven=0
        else
            echo ""
            warn "No sign-in recorded within the wait. Nothing has changed:"
            warn "  the cluster is installed, the installer's credential is still"
            warn "  live, and creating tenants stays held back."
            warn "  Sign in, then run: ./install.sh --only E-04"
            return 0
        fi
    fi

    if (( proven != 0 || kit != 0 )); then
        warn "Refusing to revoke the bootstrap token — not ready yet:"
        (( proven != 0 )) && warn "  - no administrator has exchanged a token on this cluster yet"
        (( kit != 0 )) && warn "  - no recovery kit has been exported yet"
        warn ""
        warn "  It is currently the ONLY way to write a credential, and revoking"
        warn "  it before both are true would leave this cluster with no way in"
        warn "  if either turns out not to work — an unproven login, or a kit"
        warn "  that does not exist to fall back to."
        warn ""

        if (( proven != 0 )); then
            warn "  Sign in to the portal as the cluster administrator; the sign-in"
            warn "  performs the exchange and records it. Then:"
            warn "    ./install.sh --only E-04"
            warn ""
            warn "  If signing in records nothing, the portal predates the login-time"
            warn "  exchange — open the Admin Console and select the Credentials tab,"
            warn "  which performs the same one."
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
                warn "  Every part of the path is in place; it has just not been"
                warn "  exercised. That is the one thing this installer cannot do"
                warn "  for you — the exchange needs a human at a browser."
            fi
            warn ""
        fi

        if (( kit != 0 )); then
            warn "  Export a recovery kit — the only way back into this cluster's"
            warn "  OpenBao if the login path above turns out to be broken later:"
            warn "    ./install.sh --export-recovery-kit"
            warn "  Then:"
            warn "    ./install.sh --only E-04"
        fi
        return 0
    fi

    info "Administrator sign-in confirmed ($(_handover_proof_detail))."
    info "Recovery kit exported ($(_kit_exported_detail))."

    if ! _bao_reachable; then
        error "Cannot reach OpenBao on :8200, and no BAO_TOKEN in this shell."
        error "  Neither the ClusterIP nor a kubectl port-forward responded, so"
        error "  the token cannot be revoked from here. Nothing was changed."
        return 1
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

    # The local init file held the root token and the recovery/unseal keys —
    # both now redundant. The token above is dead; the recovery material's
    # only durable purpose was to reach this moment, because _kit_exported
    # required a kit before this function could ever run. Whatever is in that
    # kit is the copy that survives from here; this file has nothing left to
    # do and reads as a live credential to anyone who finds it, so it is
    # deleted outright rather than stripped field by field.
    #
    # There used to also be a Kubernetes Secret openbao-init this patched —
    # there is no such Secret. B-04's own check() already says so ("There is
    # no local Secret for this, and inventing one would put install state
    # back on disk"), and nothing anywhere in this repository ever creates
    # one; grepped to be sure. The patch attempt was always a silent no-op.
    # Removed here along with B-04's stale header and destroy() that made the
    # same claim.
    local init_file="${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}"
    if [[ -f "${init_file}" ]]; then
        if [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]]; then
            info "Would remove ${init_file} (dry run)."
        elif rm -f "${init_file}"; then
            info "Removed ${init_file} — its content is now only in the recovery kit."
        else
            warn "Could not remove ${init_file}."
        fi
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
