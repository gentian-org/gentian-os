#!/usr/bin/env bash
# =============================================================================
# scripts/lib/catalogue.sh — App catalogue sync, operator Helm bootstrap, and orchestrator handoff.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# report_gentian_cli_state — is the CLI present, and is it THIS checkout's?
#
# Presence alone was the whole check, and presence is the easy half. The plugin
# is a script copied into place, so a machine accumulates copies: one in
# ~/.local/bin from `make install-plugin`, an older root-owned one in
# /usr/local/bin from when the installer still put it there, and the checkout's
# own. PATH order decides which answers, nothing announces the others, and a
# `gtnctl tenants deploy` can therefore run a build that predates the cluster it
# is talking to — silently, because an out-of-date CLI does not fail, it just
# does something slightly different.
#
# Compared by content rather than by version string: two copies can declare the
# same version and differ, which is exactly what a copied script does between
# releases.
report_gentian_cli_state() {
    local repo="${SCRIPT_DIR}/scripts/kubectl-gentian"
    local installed
    installed="$(command -v kubectl-gentian 2>/dev/null || true)"

    if [[ -z "${installed}" ]]; then
        info "The gentian CLI is not on PATH. Install it with:"
        info "  make -C ${SCRIPT_DIR} install-plugin"
        return 0
    fi

    [[ -r "${repo}" ]] || return 0
    if cmp -s "${repo}" "${installed}"; then
        return 0
    fi

    warn "The gentian CLI on PATH is not this checkout's copy:"
    warn "    on PATH : ${installed}"
    warn "    checkout: ${repo}"
    warn "  Refresh it with:  make -C ${SCRIPT_DIR} install-plugin"

    # Every other copy, because the stale one is only a PATH change away from
    # being the one that runs.
    local other seen=0
    while IFS= read -r other; do
        [[ -n "${other}" && "${other}" != "${installed}" ]] || continue
        (( seen++ == 0 )) && warn "  Other copies on PATH, any of which could take over:"
        warn "    ${other}"
    done < <(type -aP kubectl-gentian 2>/dev/null || true)
}

_gentian_os_services_namespace() {
    local ns
    ns=$(kubectl get deploy gentian-os -n "$(ns_kernel control)" \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="SERVICES_NAMESPACE")].value}' 2>/dev/null || true)
    if [[ -n "$ns" ]]; then
        echo "$ns"
        return
    fi
    # Fallback must match the chart default, not the old gentian-<env> guess.
    gentian_services_namespace
}
