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
    local secret
    for secret in gentian-os-master-password gentian-registry-credentials \
                  gentian-dns-credentials gentian-smtp-credentials; do
        kubectl delete secret "$secret" -n crossplane-system \
            --ignore-not-found=true 2>/dev/null || true
    done
}
