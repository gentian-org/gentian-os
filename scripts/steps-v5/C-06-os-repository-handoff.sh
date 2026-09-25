#!/usr/bin/env bash
# step: C-06-os-repository-handoff
# phase: platform
# requires: C-05-credential-catalogue
# provides: removal of the bootstrap repo-creds bridge once Repository/gentian-os reads its credential from the vault
# mutates: deletes the bootstrap repo-creds Secret in the gitops namespace

# A-06 registers a repo-creds Secret straight from the credential the
# installer collected, because B-01's Applications read gentian-os before
# OpenBao is reachable. That Secret is the one place in this design where a
# credential is applied by the shell instead of flowing through ESO, and it is
# meant to last exactly as long as the bootstrap window.
#
# This step closes the window. It runs after C-05 because Repository/gentian-os
# cannot report credentialSatisfied until the CredentialRequirement CRD exists,
# and that CRD is C-05's. Confirm, then delete: there is never a moment with no
# working credential for the repository, so a handoff that cannot be confirmed
# leaves the bridge standing rather than removing the only thing that works.
#
# A public gentian-os has no bridge and nothing to hand over.

_v5_os_repo_bridge() { echo "argocd-repo-creds-bootstrap-gentian-os"; }

check() {
    local ns; ns="$(ns_kernel gitops)"
    if kubectl get secret "$(_v5_os_repo_bridge)" -n "${ns}" >/dev/null 2>&1; then
        return "${CHECK_MISSING}"
    fi
    # Absent because it was handed over, or absent because this cluster reads
    # a public repository and one was never registered. Both are the end
    # state this step exists to reach.
    return "${CHECK_SATISFIED}"
}

apply() {
    local ns bridge claim_ns
    ns="$(ns_kernel gitops)"
    bridge="$(_v5_os_repo_bridge)"
    claim_ns="$(ns_kernel provisioning)"

    kubectl get secret "${bridge}" -n "${ns}" >/dev/null 2>&1 || {
        info "No bootstrap repo-creds bridge for gentian-os; nothing to hand over."
        return 0
    }
    kubectl get repository.gentianos.io gentian-os -n "${claim_ns}" >/dev/null 2>&1 || {
        warn "Repository/gentian-os does not exist in ${claim_ns}; keeping the bootstrap bridge."
        warn "  Without the claim there is no AppProject source and no vault-backed"
        warn "  credential to hand over to. Check the gentian-claims Application."
        return 0
    }

    info "Waiting for Repository/gentian-os to read its credential from the vault (up to 2m)..."
    local deadline=$((SECONDS + 120))
    until [[ "$(kubectl get repository.gentianos.io gentian-os -n "${claim_ns}" \
        -o jsonpath='{.status.credentialSatisfied}' 2>/dev/null)" == "true" ]]; do
        if (( SECONDS > deadline )); then
            warn "Repository/gentian-os is not credentialSatisfied after 2m — keeping the bootstrap bridge."
            warn "  Re-run this step once the credential is in the vault:"
            warn "    ./install.sh --layout v5 --only C-06-os-repository-handoff"
            return 0
        fi
        sleep 5
    done

    info "Repository/gentian-os reads its own credential; removing the bootstrap bridge."
    kubectl delete secret "${bridge}" -n "${ns}" --ignore-not-found >/dev/null
    success "Bootstrap repo-creds bridge removed."
}

destroy() {
    # A-06 owns the bridge, including removing it on teardown. Nothing of this
    # step's own outlives it.
    return 0
}
