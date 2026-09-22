#!/usr/bin/env bash
# helm_pinned — install or upgrade one release from a versions.yaml pin into a
# kernel namespace named by function. The one way a v5 step installs a chart:
#
#   helm_pinned <release> <component> <function> [helm args...]
#
# The chart and version come from versions.yaml (the `# pins:` lint keeps the
# step and the pin in step), the namespace from kernel/namespaces.yaml, and the
# release is idempotent: an existing release at the pinned version is left
# alone, another version is upgraded, and a release installed elsewhere by a
# previous layout is reported rather than silently duplicated.

helm_pinned() {
    local release="${1:?}" component="${2:?}" fn="${3:?}"; shift 3
    local ns chart version repo
    ns="$(ns_kernel "${fn}")"
    version="$(gentian_pin "${component}" chart)"
    repo="$(gentian_pin "${component}" repo)"
    chart="$(gentian_pin "${component}" name 2>/dev/null || echo "${component}")"

    local elsewhere
    elsewhere="$(helm list -A -o json 2>/dev/null | jq -r --arg r "${release}" --arg ns "${ns}" \
        '.[] | select(.name == $r and .namespace != $ns) | .namespace' | head -1)"
    if [[ -n "${elsewhere}" ]]; then
        error "release ${release} already exists in namespace ${elsewhere}; this layout puts it in ${ns}."
        error "  This installer is for a fresh cluster. Purge first, or keep the old layout."
        return 1
    fi

    ns_ensure "${ns}"
    local args=(upgrade --install "${release}" --namespace "${ns}" --version "${version}" --wait --timeout 10m "$@")
    if [[ "${repo}" == oci://* ]]; then
        args+=("${repo}")
    else
        args+=("${chart}" --repo "${repo}")
    fi
    info "helm ${release} ← ${component} ${version} → ${ns}"
    # From a scratch directory: a bare chart name that is also a directory in
    # this checkout ("crossplane") is read by helm as that directory, --repo
    # notwithstanding. Nothing here is local, so nowhere is the right cwd.
    local scratch
    scratch="$(mktemp -d)"
    (cd "${scratch}" && _helm_retry "${args[@]}")
    local rc=$?
    rm -rf "${scratch}"
    return ${rc}
}

# helm_pinned_ok — check() half: the release is deployed in the right
# namespace at the pinned version.
helm_pinned_ok() {
    local release="${1:?}" component="${2:?}" fn="${3:?}"
    local ns version
    ns="$(ns_kernel "${fn}")"
    version="$(gentian_pin "${component}" chart)"
    # The chart column is "<name>-<version>" with the version exactly as the
    # chart publishes it — "v1.21.0" for cert-manager, "2.2.1" for Crossplane —
    # which is also how versions.yaml stores it. Compare as published.
    helm list -n "${ns}" -o json 2>/dev/null | jq -e --arg r "${release}" --arg v "${version}" \
        '.[] | select(.name == $r and .status == "deployed" and (.chart | endswith("-" + $v)))' >/dev/null
}
