#!/usr/bin/env bash
# step: B-03-argocd-bootstrap-apps
# phase: secrets
# requires: B-02-openbao-transit-init
# provides: openbao, reloader, cnpg, globals and external-dns Applications
# mutates: ArgoCD Applications in argocd

# OpenBao is deployed BY ArgoCD rather than by this installer, which is what
# keeps it inside drift detection (docs/plans §1, invariant 1).

# _bootstrap_app_object_name <template> — the Application this template creates.
#
# bootstrap_argocd_apps names TEMPLATES: apply_bootstrap_application renders
# kernel/bootstrap/chart/templates/<name>.yaml. Two of them render an
# Application whose metadata.name is something else, and addressing an
# Application by its template name finds nothing:
#
#   globals       →  gentian-globals-cluster
#   kernel-admin  →  kernel-admin-<stage>
#
# Both readers here were doing exactly that. check() asked for `globals` and
# `kernel-admin` and reported this step outstanding on a cluster where every
# Application was present and healthy. destroy() deleted the same two names,
# matched nothing, and left gentian-globals-cluster and kernel-admin-<stage>
# behind on every teardown — with --ignore-not-found making the misses silent.
#
# The install log says both names on adjacent lines, which is what gives it
# away: "application.argoproj.io/gentian-globals-cluster created" immediately
# under "Applied bootstrap Application globals".
_bootstrap_app_object_name() {
    case "$1" in
        globals)      echo "gentian-globals-cluster" ;;
        kernel-admin) echo "kernel-admin-${GENTIAN_DEPLOYMENTS_STAGE:-dev}" ;;
        *)            echo "$1" ;;
    esac
}

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
        kubectl get application "$(_bootstrap_app_object_name "$app")" \
            -n argocd >/dev/null 2>&1 || return 1
    done
    if [[ "${INSTALL_CLUSTER_INFRA}" == "1" ]]; then
        for app in reloader cnpg kernel-admin; do
            kubectl get application "$(_bootstrap_app_object_name "$app")" \
                -n argocd >/dev/null 2>&1 || return 1
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
    # Every template bootstrap_argocd_apps can render, unconditionally: a
    # teardown must remove what an earlier run created under a configuration
    # this one no longer has.
    for app in openbao reloader cnpg kernel-admin globals external-dns; do
        kubectl delete application "$(_bootstrap_app_object_name "$app")" \
            -n argocd --ignore-not-found=true 2>/dev/null || true
    done
}
