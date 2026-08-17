#!/usr/bin/env bash
# step: D-07-app-catalogue
# phase: applications
# requires: D-06-appprofiles
# provides: AppCatalogue CRD and the kubectl-gentian plugin
# mutates: AppCatalogue objects, host CLI plugin

check() {
    # The CRD, not an AppCatalogue object.
    #
    # This used to require at least one object to exist, which nothing in this
    # repository creates — AppCatalogue objects arrive with the app catalogue, not
    # with the installer, and are not in this step's `provides:`. So the condition
    # could never be met and the step reported "not satisfied → applying" on every
    # run forever, which reads as a failure it is not and is absent from the list of
    # expected non-satisfied steps in GETTING-STARTED.md.
    kubectl get crd appcatalogues.gentianos.io >/dev/null 2>&1 || return 1

    # The host CLI is best-effort in apply(): it warns rather than failing when
    # /usr/local/bin needs sudo and sudo is unavailable, so its absence must not
    # make this step permanently unsatisfied either. When one IS installed it has
    # to be current, or a stale plugin would survive an upgrade silently.
    local dst
    for dst in /usr/local/bin/kubectl-gentian "${HOME}/.local/bin/kubectl-gentian"; do
        if [[ -f "${dst}" ]]; then
            cmp -s "${SCRIPT_DIR}/scripts/kubectl-gentian" "${dst}" || return 1
        fi
    done
    return 0
}

apply() {
    install_app_catalogue
}

destroy() {
    kubectl delete appcatalogue --all -A --ignore-not-found=true 2>/dev/null || true
    _remove_host_cli || true
}
