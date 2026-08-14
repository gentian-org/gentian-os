#!/usr/bin/env bash
# step: 01-crossplane-providers
# phase: control-plane
# requires: 00-crossplane
# provides: provider-kubernetes, provider-helm, provider-vault, XRDs, Compositions
# mutates: Crossplane Provider/ProviderConfig objects, gentianos.io XRDs
# pins: crossplane

check() {
    local p
    for p in provider-kubernetes provider-helm provider-vault; do
        kubectl get provider.pkg.crossplane.io "$p" >/dev/null 2>&1 || return 1
    done
    kubectl get xrd xclusters.gentianos.io >/dev/null 2>&1
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
