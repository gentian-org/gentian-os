#!/usr/bin/env bash
# step: C-05-repository-handoff
# phase: platform
# requires: C-04-credential-catalogue
# provides: removal of each bootstrap repo-creds bridge (gentian-os, deployments) once Argo CD holds the same login from the vault
# mutates: deletes the bootstrap repo-creds Secrets in the gitops namespace

# A-06 registers a repo-creds Secret straight from the credential the
# installer collected, for each repository Argo CD reads before OpenBao can
# serve it: gentian-os, which B-01's Applications read, and the deployments
# repository, whose own Repository claim is a file inside it. Those Secrets
# are the one place in this design where a credential is applied by the shell
# instead of flowing through ESO, and they are meant to last exactly as long
# as the bootstrap window.
#
# This step closes the window, for each of them. It runs after C-04 because a
# Repository claim cannot report its credential satisfied until the
# CredentialRequirement CRD exists, and that CRD is C-04's. Confirm, then
# delete: there is never a moment with no working credential for the
# repository, so a handoff that cannot be confirmed leaves the bridge standing
# rather than removing the only thing that works.
#
# What is confirmed is the Secret Argo CD reads (repo-<name>), carrying a
# login -- not the claim's credentialSatisfied, which turns true when the
# vault path has a value and so before External Secrets has written that
# Secret. Once it is there it is the credential Argo CD uses, and the bridge
# is a second copy of the token that nothing rotates: a new token goes to the
# vault and the claim's Secret follows it, the bridge would stay as it was made.
#
# A repository that does not authenticate has no bridge and nothing to hand over.

_c05_bridge_present() {
    kubectl get secret "$(argocd_bootstrap_repo_credential_name "$1")" -n "$(ns_kernel gitops)" >/dev/null 2>&1
}

check() {
    local req
    for req in $(argocd_bridged_repositories); do
        if _c05_bridge_present "${req}"; then
            return "${CHECK_MISSING}"
        fi
    done
    # Absent because they were handed over, or absent because this cluster
    # reads public repositories and none was ever registered. Both are the end
    # state this step exists to reach.
    return "${CHECK_SATISFIED}"
}

# _c05_hand_over <requirement> -- one repository's bridge. Returns 0 whether
# or not it could be removed: a bridge left standing is reported, and is not
# a failed install.
_c05_hand_over() {
    local req="$1" ns bridge name claim_ns deadline
    ns="$(ns_kernel gitops)"
    claim_ns="$(ns_kernel provisioning)"
    name="$(_repo_credential "${req}" vault)"
    bridge="$(argocd_bootstrap_repo_credential_name "${req}")"

    _c05_bridge_present "${req}" || {
        info "No bootstrap repo-creds bridge for ${name}; nothing to hand over."
        return 0
    }
    kubectl get repository.gentianos.io "${name}" -n "${claim_ns}" >/dev/null 2>&1 || {
        warn "Repository/${name} does not exist in ${claim_ns}; keeping the bootstrap bridge."
        warn "  Without the claim there is no AppProject source and no vault-backed"
        warn "  credential to hand over to. Check the gentian-claims Application."
        return 0
    }

    # How long to wait, not what to do: an env override, like the other waits.
    local limit="${GENTIAN_REPOSITORY_HANDOFF_TIMEOUT:-120}"
    info "Waiting for Repository/${name} to give Argo CD its credential from the vault (up to ${limit}s)..."
    deadline=$((SECONDS + limit))
    until argocd_claim_repo_credential_present "${req}"; do
        if (( SECONDS > deadline )); then
            warn "Argo CD has no vault-backed credential for ${name} after ${limit}s (Secret repo-${name} in ${ns}) — keeping the bootstrap bridge."
            warn "  Repository/${name} says: $(kubectl get repository.gentianos.io "${name}" -n "${claim_ns}" \
                -o jsonpath='{.status.credentialMessage}' 2>/dev/null || true)"
            warn "  Re-run this step once the credential is in the vault:"
            warn "    ./install.sh --only C-05-repository-handoff"
            return 0
        fi
        sleep 5
    done

    info "Argo CD reads ${name} with the credential from the vault; removing the bootstrap bridge."
    kubectl delete secret "${bridge}" -n "${ns}" --ignore-not-found >/dev/null
    success "Bootstrap repo-creds bridge for ${name} removed."
}

apply() {
    local req
    for req in $(argocd_bridged_repositories); do
        _c05_hand_over "${req}"
    done
}

destroy() {
    # A-06 owns the bridges, including removing them on teardown. Nothing of
    # this step's own outlives them.
    return 0
}
