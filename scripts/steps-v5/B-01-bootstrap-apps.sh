#!/usr/bin/env bash
# step: B-01-bootstrap-apps
# phase: secrets
# requires: A-06-argocd
# provides: the gentian AppProject and the kernel Applications of kernel/bootstrap-v5/chart (reloader, cnpg, kernel-postgres, kyverno, headlamp Synced and Healthy; openbao and openbao-transit Synced, awaiting their init) and the HTTPRoutes for Argo CD and Headlamp, each in its layout namespace
# mutates: a placeholder Secret openbao-transit-unseal in the seal namespace; the vault's self-signed Issuer and Certificate in the secrets namespace; Application and AppProject objects in the gitops namespace; what they sync lands in the seal, secrets, data, admission, observability and edge namespaces
# pins: headlamp

# The chart takes the namespace layout as a value and resolves every
# destination from it; it refuses to render an Application whose function the
# layout does not have. The installer passes kernel/namespaces.yaml, the same
# file A-01 created the namespaces from.

# Synced says git and cluster agree; Healthy says the workload came up. Most
# Applications here must be both. The vault and its seal cannot be Healthy
# until they are initialised, which is the next steps' work, so for them B-01
# asks only Synced and their init steps ask Healthy. external-dns needs a
# credential that arrives with the secrets steps, so its Application is
# applied by the step after those, not here.
_v5_app_state() {
    local ns="$1" app="$2"
    kubectl get application "${app}" -n "${ns}" -o jsonpath='{.status.sync.status} {.status.health.status}' 2>/dev/null
}

_v5_delivered() {
    local ns="$1" app="$2" want="${3:-healthy}" state
    state="$(_v5_app_state "${ns}" "${app}")"
    case "${want}" in
        synced)  [[ "${state%% *}" == "Synced" ]] ;;
        *)       [[ "${state}" == "Synced Healthy" ]] ;;
    esac
}

_v5_apps_healthy() { echo "reloader cnpg kernel-postgres kyverno headlamp"; }
_v5_apps_synced()  { echo "openbao openbao-transit"; }
_v5_apps()         { echo "$(_v5_apps_healthy) $(_v5_apps_synced)"; }

_v5_render() {
    # The layout goes in as a values file under its own key; platforms.yaml
    # already is one (its top-level dnsProviders table is what the chart reads).
    local tmp
    tmp="$(mktemp -d)"
    { echo "namespaces:"; sed 's/^/  /' "${NAMESPACES_FILE}"; } > "${tmp}/namespaces.yaml"
    helm template gentian-bootstrap "${SCRIPT_DIR}/kernel/bootstrap-v5/chart" \
        -f "${tmp}/namespaces.yaml" -f "${SCRIPT_DIR}/kernel/platforms.yaml" \
        --set-string "dnsProvider=none" \
        --set-string "kernelDomain=${KERNEL_DOMAIN:-}" \
        --set-string "cluster=${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}" \
        --set-string "networkMode=${NETWORK_MODE:-tunnel}" \
        --set-string "osRepo=${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}" \
        --set-string "gentianOsBranch=${GENTIAN_OS_BRANCH:-develop}" \
        --set-string "storageClass=${STORAGE_CLASS:-}" \
        --set-string "stage=${GENTIAN_DEPLOYMENTS_STAGE:-dev}" \
        --set-string "deployments.repo=${GENTIAN_DEPLOYMENTS_REPO:-}" \
        --set-string "deployments.revision=${GENTIAN_DEPLOYMENTS_BRANCH:-main}" \
        --set-string "deployments.cluster=${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}" \
        --set-string "appsets.enabled=${V5_APPSETS:-false}" \
        --set-string "versions.headlamp.chart=$(gentian_pin headlamp chart)" \
        --set-string "versions.headlamp.repo=$(gentian_pin headlamp repo)"
    local rc=$?
    rm -rf "${tmp}"
    return ${rc}
}

check() {
    local ns app
    ns="$(ns_kernel gitops)"
    kubectl get appproject gentian -n "${ns}" >/dev/null 2>&1 || return 1
    kubectl get certificate openbao-tls -n "$(ns_kernel secrets)" >/dev/null 2>&1 || return 1
    for app in $(_v5_apps_healthy); do _v5_delivered "${ns}" "${app}" || return 1; done
    for app in $(_v5_apps_synced);  do _v5_delivered "${ns}" "${app}" synced || return 1; done
    return 0
}

apply() {
    banner "Kernel bootstrap Applications"
    # The seal's pod injects its unseal key from this Secret and cannot start
    # without it; the key does not exist until B-02 initialises the seal. A
    # placeholder lets the pod start, and B-02 replaces it.
    if ! kubectl get secret openbao-transit-unseal -n "$(ns_kernel seal)" >/dev/null 2>&1; then
        kubectl create secret generic openbao-transit-unseal -n "$(ns_kernel seal)" \
            --from-literal=unseal-key=placeholder >/dev/null
        info "placeholder openbao-transit-unseal created in $(ns_kernel seal); B-02 replaces it."
    fi
    _v5_render | kubectl apply -f -
    local ns app want
    ns="$(ns_kernel gitops)"
    for app in $(_v5_apps); do
        want=healthy
        case " $(_v5_apps_synced) " in *" ${app} "*) want=synced ;; esac
        info "waiting for ${app} to be Synced$([[ ${want} == healthy ]] && echo ' and Healthy')"
        local t=$((SECONDS + 600))
        until _v5_delivered "${ns}" "${app}" "${want}"; do
            if (( SECONDS > t )); then
                error "${app} is not as required after 10m:"
                kubectl get application "${app}" -n "${ns}" -o jsonpath='{"  sync: "}{.status.sync.status}{"  health: "}{.status.health.status}{" "}{.status.health.message}{"\n"}' 2>/dev/null
                return 1
            fi
            sleep 5
        done
        success "${app} delivered"
    done
}

destroy() {
    local ns app
    ns="$(ns_kernel gitops)"
    for app in $(_v5_apps); do
        kubectl delete application "${app}" -n "${ns}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
    kubectl delete appproject gentian -n "${ns}" --ignore-not-found >/dev/null 2>&1 || true
}
