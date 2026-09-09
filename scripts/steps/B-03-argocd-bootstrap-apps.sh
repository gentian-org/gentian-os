#!/usr/bin/env bash
# step: B-03-argocd-bootstrap-apps
# phase: secrets
# requires: B-02-openbao-transit-init
# provides: openbao, reloader, cnpg, globals and external-dns Applications
# mutates: ArgoCD Applications in argocd

# OpenBao is deployed BY ArgoCD rather than by this installer, which is what
# keeps it inside drift detection: what Argo CD deploys, Argo CD also reports on.

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
    # Existence is not the question. It was, and that is the bug: once
    # bootstrapped this reported satisfied forever, so a change to any of the
    # six templates reached a fresh install and silently never reached an
    # already-bootstrapped cluster. kernel-admin-dev sat Degraded for days on
    # two fixes that were already in the repo and had no way to arrive.
    #
    # So each Application is compared against what its template renders TODAY.
    # A subset comparison, not equality: the API server defaults fields and Argo
    # CD writes others, and a check that called those drift would re-apply on
    # every run — which is worse than the fault it replaces, because it would
    # make a satisfied step meaningless.
    local app obj rc
    local apps=(openbao globals)
    if [[ "${INSTALL_CLUSTER_INFRA}" == "1" ]]; then
        apps+=(reloader cnpg kernel-admin)
        [[ "${EXTERNAL_DNS_ENABLED:-true}" == "true" && "${DNS_PROVIDER:-none}" != "none" ]] &&
            apps+=(external-dns)
    fi

    for app in "${apps[@]}"; do
        obj="$(_bootstrap_app_object_name "${app}")"
        bootstrap_application_matches "${app}" "${obj}"; rc=$?
        case ${rc} in
            0) ;;
            1)  # Say which one and why. A step that re-applies without
                # saying what changed is the same silence this replaces,
                # moved one level along.
                info "${obj} differs from kernel/bootstrap/chart/templates/${app}.yaml:"
                bootstrap_application_drift_report "${app}" "${obj}"
                return "${CHECK_MISSING}" ;;
            # Cannot tell — no python3, an unreadable object, a render this
            # shell lacks values for. Fall back to the question this used to
            # ask, so a cluster where the comparison cannot run still gets the
            # old behaviour rather than a step that re-applies every time.
            *) kubectl get application "${obj}" -n argocd >/dev/null 2>&1 ||
                   return "${CHECK_MISSING}" ;;
        esac
    done
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
        _delete_argocd_application "$(_bootstrap_app_object_name "$app")"
    done
}
