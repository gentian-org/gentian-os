#!/usr/bin/env bash
# step: B-03-argocd-bootstrap-apps
# phase: secrets
# requires: B-02-openbao-transit-init
# provides: openbao, reloader, cnpg, globals and external-dns Applications
# mutates: ArgoCD Applications in argocd

# OpenBao is deployed BY ArgoCD rather than by this installer, which is what
# keeps it inside drift detection (docs/plans §1, invariant 1).

check() {
    # Mirrors bootstrap_argocd_apps' own conditional set exactly, not a fixed
    # guess at it: openbao and globals unconditionally; reloader, cnpg and
    # kernel-admin only when INSTALL_CLUSTER_INFRA=1; external-dns only also
    # when EXTERNAL_DNS_ENABLED=true. The old fixed two-name check both missed
    # cnpg/kernel-admin/globals/external-dns (destroy() removes all of them,
    # so a partial teardown could strand any one with nothing to notice) and
    # hard-failed reloader on a minimal install, where apply() never creates
    # it at all.
    local app
    for app in openbao globals; do
        kubectl get application "$app" -n argocd >/dev/null 2>&1 || return 1
    done
    if [[ "${INSTALL_CLUSTER_INFRA}" == "1" ]]; then
        for app in reloader cnpg kernel-admin; do
            kubectl get application "$app" -n argocd >/dev/null 2>&1 || return 1
        done
        if [[ "${EXTERNAL_DNS_ENABLED:-false}" == "true" ]]; then
            kubectl get application external-dns -n argocd >/dev/null 2>&1 || return 1
        fi
    fi
    return 0
}

apply() {
    bootstrap_argocd_apps
}

destroy() {
    local app
    # Names must match the Applications bootstrap_argocd_apps creates, which are
    # named for the chart template that renders them. cnpg-cluster became
    # kernel-admin when that template was renamed for its contents.
    for app in openbao reloader cnpg kernel-admin globals external-dns; do
        kubectl delete application "$app" -n argocd \
            --ignore-not-found=true 2>/dev/null || true
    done
}
