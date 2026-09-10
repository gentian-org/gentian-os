#!/usr/bin/env bash
# step: D-08-appprofiles
# phase: applications
# requires: D-01-operator, B-12-apps-repository
# provides: AppProfile CRs from the gentian-apps repository
# mutates: nothing — verification only; B-12-apps-repository.sh owns the
#   Repository/gentian-apps claim that actually composes the catalogue-sync
#   ApplicationSet (see crossplane/compositions/repository-default.yaml).
#   This used to apply its own second Repository/gentian-apps claim under a
#   different mechanism (scripts/lib/catalogue.sh's install_catalogue_sync)
#   for the same repo — role: apps on two claims for the same repo meant two
#   ApplicationSets (named after the claim) fighting over the same
#   Applications.

check() {
    kubectl get crd appprofiles.gentianos.io >/dev/null 2>&1 || return "${CHECK_MISSING}"

    # The Repository claim, not "is there at least one AppProfile" — profiles
    # arrive afterwards, from Argo CD, once B-12's claim composes the
    # catalogue-sync ApplicationSet. A single profile that reached the
    # cluster by some other route satisfied the old test while the catalogue
    # was not syncing at all — which surfaces as a tenant refused admission
    # for an AppProfile that exists in gentian-apps and was never installed
    # here.
    kubectl get repository.gentianos.io gentian-apps -n crossplane-system >/dev/null 2>&1
}

apply() {
    # Nothing to create: B-12-apps-repository.sh already applied
    # Repository/gentian-apps (phase B runs before phase D in every normal
    # invocation — see step_ordinal_of in scripts/lib/driver.sh, execution
    # order is the sorted filename, not the requires: header). Reaching here
    # with check() still failing means this step was targeted directly
    # (--step/--from) ahead of B-12, or B-12 itself failed — either way,
    # creating a second claim here is exactly the bug this step used to have,
    # so fail instead.
    kubectl get repository.gentianos.io gentian-apps -n crossplane-system >/dev/null 2>&1 || {
        error "Repository/gentian-apps not found. Run B-12-apps-repository first — it owns this claim."
        return 1
    }
}

destroy() {
    # Repository/gentian-apps belongs to B-12-apps-repository.sh, which tears it down
    # itself — deleting it here would be removing another step's artefact at
    # the wrong point in the reverse-teardown order.
    kubectl delete appprofile --all -A --ignore-not-found=true 2>/dev/null || true
}
