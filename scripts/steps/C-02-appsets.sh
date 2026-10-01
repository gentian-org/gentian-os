#!/usr/bin/env bash
# step: C-02-appsets
# phase: platform
# requires: B-08-seed-secrets
# provides: the kernel Postgres (kernel-postgres) Synced and Healthy, the gentian-appsets Application (kernel/appsets) Synced, and its children — the system tier's data plane, the identity values in their namespaces, the claims of the deployments repository — Synced and Healthy
# mutates: the gentian-appsets Application and the ApplicationSets it creates in the gitops namespace; what they sync

# _v5_render and _v5_delivered are B-01's; a step file is a library of verbs
# and sourcing another one is how they are shared.
# shellcheck source=scripts/steps/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps/B-01-bootstrap-apps.sh"

# tenant-postgres is first in the list and first in the sync waves: a tenant's
# desktop asks for a database the moment its Component reconciles, and an
# engine that arrives after the tenant leaves that window reporting
# DatabaseUnavailable. Waiting for it here is what makes "install.sh finished"
# mean the data plane is serving.
_v5_appsets_children() { local s="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"; echo "tenant-postgres-${s} keycloak-idp-${s} openfga-${s} gentian-claims-${s} keycloak-provider-${s}"; }

# The system-tier engines, as Crossplane names their Releases.
#
# Composed by the Cluster claim (C-01) but NOT gated there: their Helm values
# are the ConfigMaps and Secret the 08-data-plane ApplicationSet syncs, and
# that ApplicationSet is this step's. A Release therefore cannot be Ready
# before this step runs, which is why xcluster_structural_ready skips
# helm.crossplane.io -- and why the wait belongs here, after the values exist.
#
# Named by the external-name the composition sets rather than by the object
# name, which carries the XR's prefix.
_v5_engine_releases() { echo "mariadb redis minio"; }

# One engine's Ready and Synced, for a message worth reading. Synced=False is
# the common failure and it means the Release could not be reconciled at all --
# usually a values ConfigMap that is not there.
_v5_release_state() {
    local e="$1" ready synced
    ready="$(_v5_release_condition "${e}" Ready)"
    synced="$(_v5_release_condition "${e}" Synced)"
    echo "Ready=${ready:-unknown} Synced=${synced:-unknown}"
}

# One Release condition, read out of the list.
#
# The program is held in a variable at file scope rather than inline in the
# function, the way namespaces.sh holds its awk: a statement inside a
# function body that begins with an identifier reads to the step-contract
# lint as a command call, and `ann = item.get(...)` was reported as calling
# a program named ann.
_V5_RELEASE_CONDITION_PY='
import json, sys
want_name, want_cond = sys.argv[1], sys.argv[2]
doc = json.load(sys.stdin)
for item in doc.get("items", []):
    if item.get("metadata", {}).get("annotations", {}).get("crossplane.io/external-name") != want_name:
        continue
    for c in item.get("status", {}).get("conditions", []):
        if c.get("type") == want_cond:
            print(c.get("status", ""))
            sys.exit(0)
'

_v5_release_condition() {
    kubectl get release.helm.crossplane.io -o json 2>/dev/null |
        python3 -c "${_V5_RELEASE_CONDITION_PY}" "$1" "$2"
}

# Whether every engine Release reports Ready.
_v5_engines_ready() {
    local e
    for e in $(_v5_engine_releases); do
        kubectl get release.helm.crossplane.io -o jsonpath="{.items[?(@.metadata.annotations['crossplane\.io/external-name']=='${e}')].status.conditions[?(@.type=='Ready')].status}" 2>/dev/null |
            grep -q True || return 1
    done
}

check() {
    local ns app
    ns="$(ns_kernel gitops)"
    for app in $(_v5_apps_deferred); do _v5_delivered "${ns}" "${app}" || return 1; done
    _v5_delivered "${ns}" gentian-appsets || return 1
    for app in $(_v5_appsets_children); do _v5_delivered "${ns}" "${app}" || return 1; done
    _v5_engines_ready
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

    local ns app t
    ns="$(ns_kernel gitops)"

    # The kernel Postgres first: Keycloak and OpenFGA below are its readers.
    # B-01 created it and could not wait for it -- its role passwords come
    # through the store C-01 composed, from the vault B-03 initialised -- and
    # Argo CD spent its retries on it in the meantime, so it is asked for a
    # fresh sync rather than waited on.
    for app in $(_v5_apps_deferred); do
        info "waiting for ${app} to be Synced and Healthy"
        t=$((SECONDS + 900))
        until _v5_delivered "${ns}" "${app}"; do
            unstick_argo_hook_job "${ns}" "${app}"
            request_argo_sync_if_stalled "${ns}" "${app}"
            if (( SECONDS > t )); then
                error "${app} is not Synced and Healthy after 15m:"
                kubectl get application "${app}" -n "${ns}" -o jsonpath='{"  sync: "}{.status.sync.status}{"  health: "}{.status.health.status}{" "}{.status.health.message}{"\n"}' 2>/dev/null
                return 1
            fi
            sleep 10
        done
        success "${app} delivered"
    done

    V5_APPSETS=true _v5_render | kubectl apply -f - >/dev/null
    for app in gentian-appsets $(_v5_appsets_children); do
        info "waiting for ${app} to be Synced and Healthy"
        t=$((SECONDS + 900))
        until _v5_delivered "${ns}" "${app}"; do
            # Never a passive wait. An Application that spent its retries --
            # on a fresh cluster, while what it reads was still arriving --
            # does not sync again on its own, however long this loop waits.
            unstick_argo_hook_job "${ns}" "${app}"
            request_argo_sync_if_stalled "${ns}" "${app}"
            if (( SECONDS > t )); then
                error "${app} is not Synced and Healthy after 15m:"
                kubectl get application "${app}" -n "${ns}" -o jsonpath='{"  sync: "}{.status.sync.status}{"  health: "}{.status.health.status}{" "}{.status.health.message}{"\n"}' 2>/dev/null
                return 1
            fi
            sleep 10
        done
        success "${app} delivered"
    done

    # And the engines the Cluster claim composed, which could not become Ready
    # until the Applications above put their values in place. This is what
    # makes "install.sh finished" mean the data plane is serving rather than
    # that it was asked for.
    info "waiting for the system-tier engines: $(_v5_engine_releases)"
    t=$((SECONDS + 1200))
    until _v5_engines_ready; do
        if (( SECONDS > t )); then
            error "the system-tier engines are not Ready after 20m:"
            local e
            for e in $(_v5_engine_releases); do
                error "  ${e}: $(_v5_release_state "${e}")"
            done
            error "  a Release stuck Synced=False is usually its values: the ConfigMaps"
            error "  it reads are synced by the 08-data-plane ApplicationSet above."
            return 1
        fi
        sleep 10
    done
    success "the system-tier engines are serving"
}

destroy() {
    kubectl delete application gentian-appsets -n "$(ns_kernel gitops)" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
