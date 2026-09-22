#!/usr/bin/env bash
# step: B-01-bootstrap-apps
# phase: secrets
# requires: A-06-argocd
# provides: the gentian AppProject and the kernel Applications of kernel/bootstrap-v5/chart (openbao, openbao-transit, reloader, cnpg, kernel-postgres, kyverno, external-dns when a DNS provider is set), each in its layout namespace
# mutates: Application and AppProject objects in the gitops namespace; what they sync lands in the seal, secrets, data, admission and edge namespaces
# pins: openbao

# The chart takes the namespace layout as a value and resolves every
# destination from it; it refuses to render an Application whose function the
# layout does not have. The installer passes kernel/namespaces.yaml, the same
# file A-01 created the namespaces from.

_v5_apps() {
    local apps="openbao openbao-transit reloader cnpg kernel-postgres kyverno"
    [[ "${DNS_PROVIDER:-none}" != "none" ]] && apps="${apps} external-dns"
    echo "${apps}"
}

_v5_render() {
    # The layout and the DNS providers are YAML files; helm reads a values file
    # per key with --set-file only as a string, so both go in as values files
    # wrapped under their key.
    local tmp
    tmp="$(mktemp -d)"
    { echo "namespaces:"; sed 's/^/  /' "${NAMESPACES_FILE}"; } > "${tmp}/namespaces.yaml"
    { echo "dnsProviders:"; sed 's/^/  /' "${SCRIPT_DIR}/kernel/platforms.yaml"; } > "${tmp}/platforms.yaml"
    helm template gentian-bootstrap "${SCRIPT_DIR}/kernel/bootstrap-v5/chart" \
        -f "${tmp}/namespaces.yaml" -f "${tmp}/platforms.yaml" \
        --set-string "dnsProvider=${DNS_PROVIDER:-none}" \
        --set-string "kernelDomain=${KERNEL_DOMAIN:-}" \
        --set-string "osRepo=${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}" \
        --set-string "gentianOsBranch=${GENTIAN_OS_BRANCH:-develop}" \
        --set-string "storageClass=${STORAGE_CLASS:-}"
    local rc=$?
    rm -rf "${tmp}"
    return ${rc}
}

check() {
    local ns app
    ns="$(ns_kernel gitops)"
    kubectl get appproject gentian -n "${ns}" >/dev/null 2>&1 || return 1
    for app in $(_v5_apps); do
        [[ "$(kubectl get application "${app}" -n "${ns}" -o jsonpath='{.status.sync.status}' 2>/dev/null)" == "Synced" ]] || return 1
    done
    return 0
}

apply() {
    banner "Kernel bootstrap Applications"
    _v5_render | kubectl apply -f -
    local ns app
    ns="$(ns_kernel gitops)"
    for app in $(_v5_apps); do
        info "waiting for ${app} to sync"
        local t=$((SECONDS + 600))
        until [[ "$(kubectl get application "${app}" -n "${ns}" -o jsonpath='{.status.sync.status}' 2>/dev/null)" == "Synced" ]]; do
            (( SECONDS > t )) && { error "${app} did not sync within 10m"; return 1; }
            sleep 5
        done
        success "${app} synced"
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
