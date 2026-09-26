#!/usr/bin/env bash
# step: C-02-appsets
# phase: platform
# requires: B-08-seed-secrets
# provides: the gentian-appsets Application (kernel/appsets-v5) Synced, and its children — the system tier's data plane, the identity values in their namespaces, the claims of the deployments repository — Synced and Healthy
# mutates: the gentian-appsets Application and the ApplicationSets it creates in the gitops namespace; what they sync

# _v5_render and _v5_delivered are B-01's; a step file is a library of verbs
# and sourcing another one is how they are shared.
# shellcheck source=scripts/steps-v5/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps-v5/B-01-bootstrap-apps.sh"

# tenant-postgres is first in the list and first in the sync waves: a tenant's
# desktop asks for a database the moment its Component reconciles, and an
# engine that arrives after the tenant leaves that window reporting
# DatabaseUnavailable. Waiting for it here is what makes "install.sh finished"
# mean the data plane is serving.
_v5_appsets_children() { local s="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"; echo "tenant-postgres-${s} keycloak-idp-${s} openfga-${s} gentian-claims-${s} keycloak-provider-${s}"; }

check() {
    local ns app
    ns="$(ns_kernel gitops)"
    _v5_delivered "${ns}" gentian-appsets || return 1
    for app in $(_v5_appsets_children); do _v5_delivered "${ns}" "${app}" || return 1; done
}

apply() {
    banner "Application sets"

    # The signing key first: the identity ApplicationSet below deploys
    # Keycloak with the listener's key mounted, and a pod waits on a Secret
    # that is not there. The key is derived from the master password and needs
    # nothing from the cluster, so it can exist before anything reads it.
    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
    IDENTITY_NAMESPACE="$(ns_kernel authentication)" \
        GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)" \
        ensure_keycloak_listener_keypair

    V5_APPSETS=true _v5_render | kubectl apply -f - >/dev/null
    local ns app t
    ns="$(ns_kernel gitops)"
    for app in gentian-appsets $(_v5_appsets_children); do
        info "waiting for ${app} to be Synced and Healthy"
        t=$((SECONDS + 900))
        until _v5_delivered "${ns}" "${app}"; do
            if (( SECONDS > t )); then
                error "${app} is not Synced and Healthy after 15m:"
                kubectl get application "${app}" -n "${ns}" -o jsonpath='{"  sync: "}{.status.sync.status}{"  health: "}{.status.health.status}{" "}{.status.health.message}{"\n"}' 2>/dev/null
                return 1
            fi
            sleep 10
        done
        success "${app} delivered"
    done
}

destroy() {
    kubectl delete application gentian-appsets -n "$(ns_kernel gitops)" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
