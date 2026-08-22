#!/usr/bin/env bash
# step: B-06-crossplane-secrets
# phase: secrets
# requires: B-05-openbao-crossplane-auth
# provides: derived-credential Secrets consumed by the Cluster XR
# mutates: Secrets in crossplane-system

check() {
    # Every Secret this step writes, not just the first one.
    #
    # Testing gentian-os-master-password alone let nine others be absent while
    # the step reported satisfied. The cluster then ran with no
    # gentian-os-kernel-oidc-openbao while the Cluster Composition referenced
    # it, and the failure surfaced as provider-vault panicking with
    # "value is null" — three layers from the step that never created it.
    local name
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        kubectl get secret "${name}" -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
            >/dev/null 2>&1 || return "${CHECK_MISSING}"
    done < <(gentian_crossplane_secret_names)
    return 0
}

apply() {
    create_crossplane_secrets
}

destroy() {
    # Same list check() and apply() use, not a separate hardcoded one. The
    # previous list named gentian-registry-credentials, gentian-dns-credentials
    # and gentian-smtp-credentials — none of which this step, or anything else
    # in the repo, has ever created — and covered only 1 of the 10 real
    # secrets gentian_crossplane_secret_names() actually names. destroy() was
    # deleting three secrets that do not exist and leaving nine that do.
    local name
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        kubectl delete secret "${name}" -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
            --ignore-not-found=true 2>/dev/null || true
    done < <(gentian_crossplane_secret_names)
}
