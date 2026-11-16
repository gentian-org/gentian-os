#!/usr/bin/env bash
# step: B-07-crossplane-secrets
# phase: secrets
# requires: B-04-vault-auth
# provides: the derived-credential Secrets the Cluster composition reads (master password and what is derived from it), in the provisioning namespace; every derived key in the vault paths those Secrets seed
# mutates: Secrets in the provisioning namespace; the vault's gentian-os/kernel/internal paths; keys missing from the vault paths in derived_kv_paths

check() {
    kubectl get secret gentian-os-master-password -n "$(ns_kernel provisioning)" >/dev/null 2>&1 ||
        return "${CHECK_MISSING}"

    # A release can add a key to a path this step derives. Argo CD applies the
    # ExternalSecret that reads it; only this step puts the key in the vault.
    # An ExternalSecret on one of those paths that is not Ready is that key
    # missing, or the vault not answering -- either way apply() is the step
    # that would add it, and adding only what is absent changes nothing else.
    local unready
    unready="$(external_secrets_missing_derived_keys)"
    if [[ -n "${unready}" ]]; then
        warn "Not Ready, and reading a vault path this step derives keys for:"
        local es
        for es in ${unready}; do warn "  ${es}"; done
        return "${CHECK_MISSING}"
    fi
}

apply() {
    export OPENBAO_NAMESPACE CROSSPLANE_NAMESPACE GENTIAN_SYSTEM_NAMESPACE
    OPENBAO_NAMESPACE="$(ns_kernel secrets)"
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    init_openbao
    # Past handover init_openbao has no token to give. Ask for one here, on
    # the terminal, before anything reads the vault: create_crossplane_secrets
    # takes the master password from the vault when it can, and its
    # _kv_add_missing would otherwise sign in with its output hidden, so the
    # sign-in URL never shows.
    if [[ -z "${BAO_TOKEN:-}" ]]; then
        resolve_openbao_access ||
            warn "No vault token: the derived Secrets are written, but keys missing from the vault are not added."
    fi
    create_crossplane_secrets
}

destroy() {
    kubectl delete secret gentian-os-master-password -n "$(ns_kernel provisioning)" --ignore-not-found >/dev/null 2>&1 || true
}
