#!/usr/bin/env bash
# step: A-02-crossplane-providers
# phase: control-plane
# requires: A-01-crossplane
# provides: provider-kubernetes, provider-helm, provider-vault, XRDs, Compositions
# mutates: Crossplane Provider/ProviderConfig objects, gentianos.io XRDs
# pins: crossplane

check() {
    local p
    for p in provider-kubernetes provider-helm provider-vault; do
        kubectl get provider.pkg.crossplane.io "$p" >/dev/null 2>&1 || return 1
    done

    # Every XRD in the repo, not just xclusters. Testing one of six let a
    # cluster missing xsuze and xinfradata — the XRDs that compose Keycloak,
    # OpenFGA and the infra databases — report satisfied, so the step that
    # would have restored them was skipped and the failure surfaced four
    # phases later as a missing Keycloak Service.
    local f name
    for f in "${SCRIPT_DIR}"/crossplane/xrds/*.yaml; do
        [[ -f "${f}" ]] || continue
        name="$(awk '/^  name:/{print $2; exit}' "${f}")"
        [[ -n "${name}" ]] || continue
        kubectl get xrd "${name}" >/dev/null 2>&1 || return 1
    done

    # Compositions too — they are half this step's `provides:`, and an XRD with
    # no Composition admits claims it can never satisfy.
    for f in "${SCRIPT_DIR}"/crossplane/compositions/*.yaml; do
        [[ -f "${f}" ]] || continue
        name="$(awk '/^  name:/{print $2; exit}' "${f}")"
        [[ -n "${name}" ]] || continue
        kubectl get composition "${name}" >/dev/null 2>&1 || return 1
    done
    return 0
}

apply() {
    install_crossplane_providers
    # install_crossplane_providers applies a *named* subset of compositions
    # (cluster-default, the app set, tenant-default). This globs the whole
    # directory, so a composition added to the repo is picked up without being
    # added to a second list — the divergence that made update.sh's
    # op_crossplane_update necessary in the first place. Re-applying the named
    # ones is idempotent.
    apply_crossplane_platform_compositions_update
}

destroy() {
    _delete_provider_config "providerconfig.kubernetes.crossplane.io/kubernetes" "provider-kubernetes/kubernetes" || true
    _delete_provider_config "providerconfig.helm.crossplane.io/kubernetes"       "provider-helm/kubernetes" || true
    _delete_provider_config "providerconfig.vault.upbound.io/openbao"            "provider-vault/openbao" || true

    local p
    for p in provider-kubernetes provider-helm provider-vault; do
        kubectl delete provider.pkg.crossplane.io "$p" --ignore-not-found=true --timeout=60s 2>/dev/null || true
    done
    _delete_crossplane_crds || true
}
