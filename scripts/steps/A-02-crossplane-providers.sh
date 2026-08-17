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

# _argocd_owns_crossplane_platform — have the XRD/Composition Applications been
# created yet?
#
# kernel/appsets/raw/03-crossplane-platform.yaml defines these two over
# crossplane/xrds and crossplane/compositions with prune and selfHeal, which makes
# Git the authority for both directories from the moment they exist.
_argocd_owns_crossplane_platform() {
    local a
    for a in crossplane-xrds crossplane-compositions; do
        kubectl get application "${a}" -n argocd >/dev/null 2>&1 || return 1
    done
    return 0
}

apply() {
    # Providers stay unconditional: they are this step's own `provides:` and no
    # Application manages them.
    install_crossplane_providers

    # XRDs and Compositions are applied from the local working tree ONLY before
    # Argo CD is managing them.
    #
    # Once crossplane-xrds and crossplane-compositions exist, Git is the authority
    # and this would apply whatever happens to be checked out over what Git says —
    # then selfHeal reverts it, and a re-run does it again. That is how a cluster
    # ends up matching the last person's working tree rather than any commit, and
    # it is the same double-writer shape that kept gentian-appsets OutOfSync.
    #
    # Deliberately no kubectl-triggered refresh here: if Argo CD owns them and has
    # not delivered them, check() stays unsatisfied and the driver reports THIS
    # step — which is the honest signal — rather than the installer papering over a
    # sync that is not happening.
    if _argocd_owns_crossplane_platform; then
        info "Argo CD owns crossplane/xrds and crossplane/compositions" \
             "(Applications crossplane-xrds, crossplane-compositions) — not applying from the local tree."
        return 0
    fi

    # Bootstrap only. install_crossplane_providers applies a *named* subset of
    # compositions (cluster-default, the app set, tenant-default). This globs the
    # whole directory, so a composition added to the repo is picked up without being
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

    # The bindings that grant each provider cluster-admin. Crossplane's package
    # manager garbage-collects them only while it is running, and it is gone by
    # the time A-01 finishes — so they outlive the providers they belong to,
    # still naming a ServiceAccount any later workload could occupy.
    kubectl delete clusterrolebinding \
        crossplane-provider-helm-admin \
        crossplane-provider-kubernetes-admin \
        crossplane-provider-vault-admin \
        --ignore-not-found=true --wait=false 2>/dev/null || true

    _delete_crossplane_crds || true
}
