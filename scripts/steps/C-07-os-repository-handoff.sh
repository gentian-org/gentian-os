#!/usr/bin/env bash
# step: C-07-os-repository-handoff
# phase: platform
# requires: C-06-credential-catalogue
# provides: deletion of argocd-repo-creds-bootstrap-os once Repository/os is credentialSatisfied from OpenBao
# mutates: deletes a Secret in argocd

# Split out of B-11-os-repository (see its header): Repository/os's
# CredentialRequirement cannot compose until the credentialrequirements.gentianos.io
# CRD exists, and that CRD is installed by C-06-credential-catalogue — a whole
# phase after B-11 applies the claim. Waiting for credentialSatisfied inside
# B-11 meant waiting on a CRD that could not possibly exist yet: guaranteed
# timeout, every run, regardless of OpenBao or ESO health.
#
# Running after C-06 fixes the ordering without touching B-11's position:
# apply-and-move-on there (matching B-09/B-12/B-13), confirm-then-delete here.
# Still confirm-then-delete, so there is never a window with zero working
# credential for osRepo — this step only removes the A-09 bootstrap bridge
# Secret once Repository/os proves it no longer needs it.

check() {
    ! kubectl get secret argocd-repo-creds-bootstrap-os -n argocd >/dev/null 2>&1
}

apply() {
    kubectl get secret argocd-repo-creds-bootstrap-os -n argocd >/dev/null 2>&1 || return 0
    kubectl get repository.gentianos.io os -n crossplane-system >/dev/null 2>&1 || return 0

    info "Waiting for Repository/os credential to be satisfied from OpenBao (up to 2m)..."
    local i=0
    until [[ "$(kubectl get repository.gentianos.io os -n crossplane-system -o jsonpath='{.status.credentialSatisfied}' 2>/dev/null)" == "true" ]]; do
        echo -n "."
        sleep 5; i=$((i + 5))
        [[ $i -lt 120 ]] || {
            warn "Repository/os not yet credentialSatisfied after 2m — leaving the bootstrap bridge Secret in place; re-run install.sh to retry the handoff."
            echo ""
            return 0
        }
    done
    echo ""

    info "Repository/os credential satisfied from OpenBao — removing the bootstrap ArgoCD repo-creds bridge."
    kubectl delete secret argocd-repo-creds-bootstrap-os -n argocd --ignore-not-found=true
}

destroy() {
    # Nothing of this step's own to tear down — B-11's destroy() removes the
    # Repository claim, and the bootstrap bridge Secret this step deletes is
    # already gone in the common case by the time a teardown runs.
    return 0
}
