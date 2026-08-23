#!/usr/bin/env bash
# step: D-08-app-catalogue
# phase: applications
# requires: D-07-appprofiles
# provides: the AppCatalogue CRD, confirmed present
# mutates: AppCatalogue objects (on the reverse pass only)

# This step no longer installs anything, on the cluster or on the host.
#
# The CRD ships with the operator chart, which the gentian-os Application syncs
# at wave 0; applying it again from config/crd/ in this checkout made the
# installer a second writer of an Argo-owned object. And the catalogue is
# populated by neither: AppProfiles arrive from gentian-apps through the
# gentian-catalogue ApplicationSet, and the operator's appstore controller
# builds the AppCatalogue singleton from them.
#
# The kubectl-gentian/gtnctl install left with it. A cluster installer should not
# be asking for root on the machine it runs from, and an uninstall should not
# delete the binary that drives every other cluster the operator manages. It is
# `make install-plugin` now, and GETTING-STARTED says so.

check() {
    # The CRD, not an AppCatalogue object.
    #
    # This used to require at least one object to exist, which nothing in this
    # repository creates — the operator's appstore controller does, in-cluster.
    # So the condition could never be met and the step reported "not satisfied →
    # applying" on every run forever, which reads as a failure it is not.
    #
    # It also used to compare the host binaries against the source tree, which
    # made a cluster step's verdict depend on the operator's laptop.
    kubectl get crd appcatalogues.gentianos.io >/dev/null 2>&1
}

apply() {
    install_app_catalogue
}

destroy() {
    # The objects, not the CRD: that belongs to the operator chart and goes when
    # Argo CD removes it. Nothing here touches the host — use
    # `make uninstall-plugin` for that, on the machine that wants it gone.
    kubectl delete appcatalogue --all -A --ignore-not-found=true 2>/dev/null || true
}
