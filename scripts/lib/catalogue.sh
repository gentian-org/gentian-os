#!/usr/bin/env bash
# =============================================================================
# scripts/lib/catalogue.sh — App catalog sync, operator Helm bootstrap, and orchestrator handoff.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# report_gentian_cli_state — is the CLI present, and is it THIS checkout's?
#
# Presence alone was the whole check, and presence is the easy half. The plugin
# is a script copied into place, so a machine accumulates copies: one in
# ~/.local/bin from `make install-plugin`, an older root-owned one in
# /usr/local/bin from when the installer still put it there, and the checkout's
# own. PATH order decides which answers, nothing announces the others, and a
# `gtnctl tenants deploy` can therefore run a build that predates the cluster it
# is talking to — silently, because an out-of-date CLI does not fail, it just
# does something slightly different.
#
# Compared by content rather than by version string: two copies can declare the
# same version and differ, which is exactly what a copied script does between
# releases.
report_gentian_cli_state() {
    local repo="${SCRIPT_DIR}/scripts/kubectl-gentian"
    local installed
    installed="$(command -v kubectl-gentian 2>/dev/null || true)"

    if [[ -z "${installed}" ]]; then
        info "The gentian CLI is not on PATH. Install it with:"
        info "  make -C ${SCRIPT_DIR} install-plugin"
        return 0
    fi

    [[ -r "${repo}" ]] || return 0
    # Without the two lines install-plugin writes into the copy (where it came
    # from, and at which commit): those differ by design.
    if cmp -s <(grep -v '^GENTIAN_PLUGIN_\(SOURCE\|COMMIT\)=' "${repo}") \
              <(grep -v '^GENTIAN_PLUGIN_\(SOURCE\|COMMIT\)=' "${installed}"); then
        return 0
    fi

    warn "The gentian CLI on PATH is not this checkout's copy:"
    warn "    on PATH : ${installed}"
    warn "    checkout: ${repo}"
    warn "  Refresh it with:  make -C ${SCRIPT_DIR} install-plugin"

    # Every other copy, because the stale one is only a PATH change away from
    # being the one that runs.
    local other seen=0
    while IFS= read -r other; do
        [[ -n "${other}" && "${other}" != "${installed}" ]] || continue
        (( seen++ == 0 )) && warn "  Other copies on PATH, any of which could take over:"
        warn "    ${other}"
    done < <(type -aP kubectl-gentian 2>/dev/null || true)
}

_gentian_os_services_namespace() {
    local ns
    ns=$(kubectl get deploy gentian-os -n "$(ns_kernel control)" \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="SERVICES_NAMESPACE")].value}' 2>/dev/null || true)
    if [[ -n "$ns" ]]; then
        echo "$ns"
        return
    fi
    # Fallback must match the chart default, not the old gentian-<env> guess.
    gentian_services_namespace
}

# =============================================================================
# The installer's default profiles (AD-14).
#
# A profile reaches a cluster through the director, fetched from a catalog
# and held to a digest. One arrives earlier than any director: the Operations
# Console, which step 0 places in clusters/<cluster>/catalogue/ so that the
# cluster comes up with it. This is that path, and it is held to the same
# rule: the bytes are refused unless they hash to a digest somebody stated --
# the catalog's own index, or the person installing, who may pin one -- and
# what is written is what the director writes for an install of the same
# bundle, byte for byte (internal/director/gitops/catalogue.go,
# MaterialiseProfile): the bundle as served, the patch that puts those bytes
# and the catalog's origin on the profile, and both in the kustomization.
# A test holds the two to each other (installer_profiles_test.go).
# =============================================================================

# The largest bundle that can be carried beside its profile
# (internal/profilebundle, MaxBytes).
_PROFILE_BUNDLE_MAX_BYTES=184320

# The profiles the platform's own chart ships. A catalog entry of one of
# these names is refused by the director (gitops.PlatformProfile), and here.
_PLATFORM_PROFILE_NAMES="admin-console concierge desktop"

# _yaml_docs_json <file> -- every YAML document of the file as one line of
# JSON, with either flavor of yq.
_yaml_docs_json() {
    local out
    if out="$(yq eval -o=json -I=0 '.' "$1" 2>/dev/null)" && [[ -n "${out}" ]]; then
        printf '%s\n' "${out}"; return 0
    fi
    if out="$(yq -c '.' "$1" 2>/dev/null)" && [[ -n "${out}" ]]; then
        printf '%s\n' "${out}"; return 0
    fi
    return 1
}

# _catalogue_fetch <https address> <file> -- one GET, into the file.
#
#   0  served (200)
#   2  the catalog cannot be reached: no answer, or a 5xx
#   3  it answers and does not serve this (the status is in
#      _CATALOGUE_FETCH_CODE)
#
# https and nothing else, and a redirect is not followed -- the address that
# is written down is the one that is asked, as the director has it.
_CATALOGUE_FETCH_CODE=""
_catalogue_fetch() {
    local url="$1" out="$2" code="" rc=0
    code="$(curl -sS --proto '=https' --max-time 30 --max-filesize 1048576 \
        -o "${out}" -w '%{http_code}' "${url}" 2>/dev/null)" || rc=$?
    _CATALOGUE_FETCH_CODE="${code:-000}"
    if [[ "${rc}" != "0" || "${_CATALOGUE_FETCH_CODE}" == "000" || "${_CATALOGUE_FETCH_CODE}" == 5* ]]; then
        return 2
    fi
    [[ "${_CATALOGUE_FETCH_CODE}" == "200" ]] && return 0
    return 3
}

# _catalogue_index_digest <index file> <name> -- the digest the index lists
# for an entry, as sha256:<lowercase hex>, or nothing.
_catalogue_index_digest() {
    local digest
    digest="$(_yaml_docs_json "$1" 2>/dev/null | jq -r --arg n "$2" \
        '(.entries // [])[]? | select((.name // "") == $n) | .digest // empty' 2>/dev/null | head -1 || true)"
    digest="$(printf '%s' "${digest}" | tr 'A-F' 'a-f' | tr -d '[:space:]')"
    [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || return 0
    printf '%s' "${digest}"
}

# _profile_bundle_refusal <bundle file> <name> -- why this file is not a
# bundle the installer places, or nothing when it is.
#
# What the director asks of a bundle from a catalog of the whole cluster
# (internal/profilebundle, Check), as far as a shell can ask it: one
# ComponentProfile of this name, first, stating nothing only the platform
# records and nothing addressed to Argo CD; after it only the four kinds a
# bundle may hold, each under the one name this profile gives it, stating a
# name, labels and its one body and nothing else. The rules inside a
# companion's body are the director's and the operator's to check.
_profile_bundle_refusal() {
    local docs
    if ! docs="$(_yaml_docs_json "$1")"; then
        printf 'it does not parse as YAML'
        return 0
    fi
    printf '%s\n' "${docs}" | jq -rs --arg name "$2" '
        def meta: (.metadata // {});
        def body_of: {"Composition":"spec","OIDCPackCatalog":"spec","ConfigMap":"data","Customization":"spec"}[.kind];
        def allowed: (.apiVersion // "") + "/" + (.kind // "")
            | IN("apiextensions.crossplane.io/v1/Composition", "gentianos.io/v1alpha1/OIDCPackCatalog",
                 "v1/ConfigMap", "gentianos.io/v1alpha1/Customization");
        def wanted_name($n):
            if .kind == "Composition" then (meta.name == "app-" + $n)
            elif .kind == "OIDCPackCatalog" then (meta.name == $n + "-oidc")
            else ((meta.name // "") | startswith($n + ".") and length > ($n | length) + 1) end;
        def companion($n):
            if (type != "object") then "a document is not an object"
            elif .kind == "ComponentProfile" then "it holds a second ComponentProfile"
            elif (allowed | not) then "it holds a \(.kind // "nothing") (\(.apiVersion // "nothing")), which is not a kind a bundle may hold"
            elif (wanted_name($n) | not) then "its \(.kind) is named \(meta.name // "nothing"), which is not a name profile \($n) gives one"
            elif meta.name == "app-default" or meta.name == "kube-root-ca.crt" then "it holds the \(.kind) \(meta.name), which is the platform'"'"'s"
            elif ((meta | keys) - ["name", "labels"] | length) > 0 then "the \(.kind) \(meta.name) states more of its metadata than a name and labels"
            elif ((keys - ["apiVersion", "kind", "metadata", body_of]) | length) > 0 then "the \(.kind) \(meta.name) states more than its \(body_of)"
            elif (meta.labels // {})["gentianos.io/profile-name"] != $n then "the \(.kind) \(meta.name) does not carry the label gentianos.io/profile-name: \($n)"
            elif .kind == "Composition" and ((.spec.compositeTypeRef.apiVersion // "") != "gentianos.io/v1alpha1" or (.spec.compositeTypeRef.kind // "") != "XApp")
                then "the Composition \(meta.name) composes something other than an XApp"
            else empty end;
        map(select(. != null)) as $d
        | if ($d | length) == 0 then "it holds no document"
          elif ($d[0] | type) != "object" or $d[0].kind != "ComponentProfile" then "its first document is not a ComponentProfile"
          elif ($d[0] | meta.name) != $name then "its profile is named \($d[0] | meta.name // "nothing") in the bundle, not \($name)"
          elif (($d[0].apiVersion // "") | test("^gentianos\\.io/v[a-z0-9]+$") | not) then "its profile states the apiVersion \($d[0].apiVersion // "nothing")"
          elif (($d[0] | meta.namespace // "") | test("^[a-z0-9.-]*$") | not) then "its profile states a namespace that is not a name"
          elif ($d[0] | meta.annotations // {} | has("gentianos.io/profile-bundle") or has("gentianos.io/catalogue-origin"))
              then "its profile states an annotation only the platform writes"
          elif ($d[0] | [(meta.labels // {} | keys[]), (meta.annotations // {} | keys[])] | any(startswith("argocd.argoproj.io/")))
              then "its profile states something addressed to Argo CD"
          elif ($d[0] | (meta.labels // {}) | has("gentianos.io/profile-name") and .["gentianos.io/profile-name"] != $name)
              then "its profile carries another profile'"'"'s name as its label"
          elif ($d | length) > 1 and ($name | test("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$") | not) then "a profile that brings companions has a short lower-case name"
          elif ([$d[1:][] | "\(.kind)/\(meta.name)"] | length != (unique | length)) then "it holds one companion twice"
          else ([$d[1:][] | companion($name)] | first // "") end
    ' 2>/dev/null || printf 'it could not be read'
}

# _profile_bundle_recorded <catalogue dir> <name> -- what the cluster's
# definition records for a profile: "<digest> <origin>", read from
# <name>.bundle.yaml the way the director reads it (the digest is that of the
# bytes the file carries, not a number written beside them). Nothing when the
# profile has no bundle file.
_profile_bundle_recorded() {
    local file="$1/$2.bundle.yaml" json encoded origin tmp digest
    [[ -f "${file}" ]] || return 0
    json="$(_yaml_docs_json "${file}" 2>/dev/null | head -1 || true)"
    encoded="$(printf '%s' "${json}" | jq -r '.metadata.annotations["gentianos.io/profile-bundle"] // empty' 2>/dev/null || true)"
    origin="$(printf '%s' "${json}" | jq -r '.metadata.annotations["gentianos.io/catalogue-origin"] // empty' 2>/dev/null || true)"
    digest=""
    if [[ -n "${encoded}" ]]; then
        tmp="$(mktemp)"
        if printf '%s' "${encoded}" | openssl base64 -d -A > "${tmp}" 2>/dev/null; then
            digest="sha256:$(sha256_of "${tmp}")"
        fi
        rm -f "${tmp}"
    fi
    printf '%s %s' "${digest:-unreadable}" "${origin:-none}"
}

# _kustomization_list <file> resource|patch <entry file> -- add a file to the
# kustomization's resources or patches, exactly where and how the director
# adds one (internal/director/gitops: ensureResourceListed,
# ensurePatchListed). A line editor, as there: the file is somebody's.
_kustomization_list() {
    local file="$1" mode="$2" entry tmp
    entry="- $3"
    [[ "${mode}" == "patch" ]] && entry="- path: $3"
    tmp="$(mktemp)"
    awk -v entry="${entry}" -v mode="${mode}" '
        function trim(s) { gsub(/^[ \t\r]+|[ \t\r]+$/, "", s); return s }
        { lines[++n] = $0 }
        END {
            for (i = 1; i <= n; i++) if (trim(lines[i]) == entry) { for (j = 1; j <= n; j++) print lines[j]; exit }
            while (n > 0 && lines[n] == "") n--
            key = (mode == "patch") ? "patches:" : "resources:"
            at = 0
            for (i = 1; i <= n; i++) {
                if (mode != "patch" && trim(lines[i]) == "resources: []") lines[i] = "resources:"
                if (trim(lines[i]) == key) { at = i; break }
            }
            if (at == 0) {
                for (j = 1; j <= n; j++) print lines[j]
                print key; print entry; exit
            }
            end = at + 1
            while (end <= n) {
                t = trim(lines[end])
                if (mode == "patch") { if (t == "" || (substr(t, 1, 1) != "-" && substr(lines[end], 1, 1) != " ")) break }
                else if (substr(t, 1, 1) != "-") break
                end++
            }
            for (j = 1; j < end; j++) print lines[j]
            print entry
            for (j = end; j <= n; j++) print lines[j]
        }' "${file}" > "${tmp}" || { rm -f "${tmp}"; return 1; }
    if ! cmp -s "${tmp}" "${file}"; then cat "${tmp}" > "${file}"; fi
    rm -f "${tmp}"
}

# gentian_materialise_profile <catalogue dir> <name> <bundle file> <origin>
#
# Writes one verified bundle into the cluster's catalog directory as the
# director's MaterialiseProfile does: <name>.yaml, the bytes unchanged;
# <name>.bundle.yaml, the patch that carries them and the origin on the
# profile; builds/<name>-<12 hex of the digest>.yaml, the ProfileBuild that
# keeps this build for a tenant that runs it after the profile moved on; and
# all three listed in kustomization.yaml. The caller has checked the digest
# and the bundle; nothing here fetches or decides.
gentian_materialise_profile() {
    local dir="$1" name="$2" body="$3" origin="$4" head api ns tmp digest build
    head="$(_yaml_docs_json "${body}" | jq -cs 'map(select(. != null)) | .[0]')" || return 1
    api="$(printf '%s' "${head}" | jq -r '.apiVersion // empty')"
    ns="$(printf '%s' "${head}" | jq -r '.metadata.namespace // empty')"
    [[ -n "${api}" ]] || { error "${name} states no apiVersion."; return 1; }

    mkdir -p "${dir}"
    if [[ ! -f "${dir}/kustomization.yaml" ]] || ! grep -q '[^[:space:]]' "${dir}/kustomization.yaml"; then
        printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n' > "${dir}/kustomization.yaml"
    fi
    cmp -s "${body}" "${dir}/${name}.yaml" 2>/dev/null || cat "${body}" > "${dir}/${name}.yaml"

    tmp="$(mktemp)"
    {
        printf '# The bytes of %s.yaml as the catalogue source served them, for the\n' "${name}"
        printf '# operator to check a pinned install against. Written by the director with\n'
        printf '# the profile; do not edit either without the other.\n'
        printf 'apiVersion: %s\n' "${api}"
        printf 'kind: ComponentProfile\n'
        printf 'metadata:\n'
        printf '  name: %s\n' "${name}"
        if [[ -n "${ns}" ]]; then printf '  namespace: %s\n' "${ns}"; fi
        printf '  annotations:\n'
        printf '    gentianos.io/profile-bundle: "%s"\n' "$(openssl base64 -A < "${body}")"
        printf '    gentianos.io/catalogue-origin: %s\n' "${origin}"
    } > "${tmp}"
    cmp -s "${tmp}" "${dir}/${name}.bundle.yaml" 2>/dev/null || cat "${tmp}" > "${dir}/${name}.bundle.yaml"
    rm -f "${tmp}"

    # The build, byte for byte what internal/director/gitops renderBuild writes.
    digest="sha256:$(sha256_of "${body}")"
    build="${name}-${digest:7:12}"
    mkdir -p "${dir}/builds"
    tmp="$(mktemp)"
    {
        printf '# One build of %s: the bundle at %s, kept for every tenant\n' "${name}" "${digest:0:19}"
        printf '# that runs it after the profile moved on. Written by the director; a build is\n'
        printf '# its digest, so nothing edits it.\n'
        printf 'apiVersion: gentianos.io/v1alpha1\n'
        printf 'kind: ProfileBuild\n'
        printf 'metadata:\n'
        printf '  name: %s\n' "${build}"
        printf '  labels:\n'
        printf '    gentianos.io/profile-name: %s\n' "${name}"
        printf '    gentianos.io/profile-build: %s\n' "${build}"
        printf 'spec:\n'
        printf '  profile: %s\n' "${name}"
        printf '  digest: %s\n' "${digest}"
        printf '  origin: %s\n' "${origin}"
        printf '  bundle: "%s"\n' "$(openssl base64 -A < "${body}")"
    } > "${tmp}"
    cmp -s "${tmp}" "${dir}/builds/${build}.yaml" 2>/dev/null || cat "${tmp}" > "${dir}/builds/${build}.yaml"
    rm -f "${tmp}"

    _kustomization_list "${dir}/kustomization.yaml" resource "${name}.yaml" || return 1
    _kustomization_list "${dir}/kustomization.yaml" patch "${name}.bundle.yaml" || return 1
    _kustomization_list "${dir}/kustomization.yaml" resource "builds/${build}.yaml" || return 1
}

# _default_profile_origin <catalogue address> <cluster> -- the origin recorded
# for a profile fetched from this address: "cluster/<source>".
#
# The name of the catalog the Cluster claim declares at that address, when
# it declares one -- then the record is the one an install through the
# director from that catalog leaves. Otherwise a name made of the address
# itself, so that the record still says where the bytes came from.
_default_profile_origin() {
    local base="${1%/}" cluster="$2" claim name=""
    claim="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel/claims/cluster.yaml"
    if [[ -f "${claim}" ]]; then
        name="$(_yaml_docs_json "${claim}" 2>/dev/null | jq -r --arg u "${base}" \
            'select(. != null) | (.spec.catalogue.sources // [])[]? | select(((.url // "") | rtrimstr("/")) == $u) | .name // empty' \
            2>/dev/null | head -1 || true)"
    fi
    if [[ ! "${name}" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]; then
        name="$(_address_origin_name "${base}")"
    fi
    printf 'cluster/%s' "${name}"
}

# _address_origin_name <catalogue address> -- the source name made of the
# address itself, for a catalog the Cluster claim does not name.
_address_origin_name() {
    printf '%s' "${1#https://}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' | sed 's/--*/-/g; s/^-//; s/-$//'
}

# The store's extra services: the profiles an installation places from the
# store's catalog unless --no-extra-services says otherwise. Where each runs
# is the profile's own to say (its placement): the Operations Console in the
# platform tenant, the App Store app and the Aluvian Center in every other
# tenant while the cluster offers a store, tenants made later included.
_EXTRA_SERVICE_PROFILES="operations-console-ee app-store-pe aluvian-center-pe"

# Names the extra services were placed under before the catalog renamed
# them. A profile an earlier run placed under one of them from the store's
# catalog leaves the cluster's catalog on the next run, as the extra
# services do with --no-extra-services: the catalog no longer lists it, so
# it would otherwise stay at its last build for good, and the Components the
# operator placed of it with it. One placed from anywhere else stays.
_RETIRED_EXTRA_SERVICE_PROFILES="operations-console app-store subscription-center"

# _default_profiles_configured [cluster] -- the list, as GENTIAN_DEFAULT_PROFILES
# gives it or, unset, the extra services at the store's catalog. Without the
# extra services (gentian_extra_services_enabled) none of the three is in it,
# whichever way the list was given.
_default_profiles_configured() {
    local cluster="${1:-}" base list name entry out="" claim=""
    base="${GENTIAN_STORE_CATALOGUE_URL:-https://catalogue.aluvian.io}"
    base="${base%/}"
    list="${GENTIAN_DEFAULT_PROFILES-}"
    if [[ -z "${GENTIAN_DEFAULT_PROFILES+set}" ]]; then
        for name in ${_EXTRA_SERVICE_PROFILES}; do
            list="${list:+${list},}${base}/profiles/${name}.yaml"
        done
    fi
    [[ -n "${cluster}" ]] && claim="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel/claims/cluster.yaml"
    if [[ "$(gentian_extra_services_enabled "${claim}")" == "true" ]]; then
        printf '%s' "${list}"
        return 0
    fi
    local -a entries=()
    IFS=',' read -r -a entries <<< "${list}"
    for entry in ${entries[@]+"${entries[@]}"}; do
        _default_profile_is_extra_service "${entry}" && continue
        out="${out:+${out},}${entry}"
    done
    printf '%s' "${out}"
}

# _default_profile_is_extra_service <entry> -- whether the entry names one of
# the extra services' profiles (<catalogue>/profiles/<name>.yaml[@pin]).
_default_profile_is_extra_service() {
    local item name extra
    item="$(printf '%s' "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
    item="${item%@*}"
    name="${item##*/}"
    name="${name%.yaml}"
    for extra in ${_EXTRA_SERVICE_PROFILES}; do
        [[ "${name}" == "${extra}" ]] && return 0
    done
    return 1
}

# _store_catalogue_base -- the store's catalog, without a trailing slash.
_store_catalogue_base() {
    printf '%s' "${GENTIAN_STORE_CATALOGUE_URL:-https://catalogue.aluvian.io}" | sed 's:/*$::'
}

# _is_store_catalogue_origin <cluster> <origin> -- whether a recorded origin
# is the store's catalogue: under the name the Cluster claim gives it, or
# under the one made of its address, which a run before the claim named it
# recorded.
_is_store_catalogue_origin() {
    local base
    base="$(_store_catalogue_base)"
    [[ "$2" == "$(_default_profile_origin "${base}" "$1")" || "$2" == "cluster/$(_address_origin_name "${base}")" ]]
}

# _retired_extra_services_remove <cluster> <catalogue dir> -- the profiles
# placed under a name the extra services no longer have
# (_RETIRED_EXTRA_SERVICE_PROFILES) leave the cluster's catalog, when their
# recorded origin is the store's catalog.
_retired_extra_services_remove() {
    local cluster="$1" dir="$2" name recorded names=""
    for name in ${_RETIRED_EXTRA_SERVICE_PROFILES}; do
        [[ -f "${dir}/${name}.yaml" ]] || continue
        recorded="$(_profile_bundle_recorded "${dir}" "${name}")"
        _is_store_catalogue_origin "${cluster}" "${recorded#* }" || continue
        names="${names:+${names} }${name}"
    done
    [[ -n "${names}" ]] || return 0
    _default_extra_services_remove "${dir}" "${names}" quiet
    info "Removed ${names} from the cluster's catalogue: the store's catalogue no longer has the extra service under that name."
}

# _default_extra_services_remove <catalogue dir> [names] [quiet] -- without the
# extra services, the profiles an earlier run placed for them leave the
# cluster's catalogue: the profile, its bundle patch and their lines in the
# kustomization. The ProfileBuilds stay: a tenant that runs a build keeps it.
# The Components the operator placed of them go once the profiles do
# (ensureDefaultComponents).
_default_extra_services_remove() {
    local dir="$1" names="${2:-${_EXTRA_SERVICE_PROFILES} ${_RETIRED_EXTRA_SERVICE_PROFILES}}" quiet="${3:-}" name kust tmp removed=""
    kust="${dir}/kustomization.yaml"
    for name in ${names}; do
        [[ -f "${dir}/${name}.yaml" ]] || continue
        rm -f "${dir}/${name}.yaml" "${dir}/${name}.bundle.yaml"
        if [[ -f "${kust}" ]]; then
            tmp="$(mktemp)"
            awk -v a="- ${name}.yaml" -v b="- path: ${name}.bundle.yaml" '
                function trim(s) { gsub(/^[ \t\r]+|[ \t\r]+$/, "", s); return s }
                trim($0) == a || trim($0) == b { next }
                { print }
            ' "${kust}" > "${tmp}" && cat "${tmp}" > "${kust}"
            rm -f "${tmp}"
        fi
        removed="${removed:+${removed} }${name}"
    done
    [[ -n "${removed}" ]] || return 0
    # A list left with no entry is written as an empty one.
    if [[ -f "${kust}" ]]; then
        tmp="$(mktemp)"
        awk '
            { lines[++n] = $0 }
            END {
                for (i = 1; i <= n; i++) {
                    t = lines[i]; gsub(/[ \t\r]+$/, "", t)
                    if ((t == "resources:" || t == "patches:") && (i == n || lines[i + 1] !~ /^[ \t]*-/)) lines[i] = t " []"
                    print lines[i]
                }
            }
        ' "${kust}" > "${tmp}" && cat "${tmp}" > "${kust}"
        rm -f "${tmp}"
    fi
    [[ -n "${quiet}" ]] || info "Extra services off (--no-extra-services): removed ${removed} from the cluster's catalogue."
}

# _default_profile_parse <entry> -- reads one entry of the list into
# _DP_URL, _DP_BASE, _DP_NAME and _DP_PIN, or says why it is not one.
#
# An entry is the https address of a profile in a catalog,
# <catalogue>/profiles/<name>.yaml, optionally followed by @sha256:<digest>.
# A local file was accepted once and is not any more: it has no catalog to
# record as its origin and no index to state its digest, and a file on the
# install host is exactly the unverified copy this path no longer writes.
_DP_URL=""; _DP_BASE=""; _DP_NAME=""; _DP_PIN=""
_default_profile_parse() {
    local item="$1" reserved
    _DP_URL=""; _DP_BASE=""; _DP_NAME=""; _DP_PIN=""
    if [[ "${item}" == *@* ]]; then
        _DP_PIN="$(printf '%s' "${item##*@}" | tr 'A-F' 'a-f')"
        item="${item%@*}"
        if [[ ! "${_DP_PIN}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
            error "GENTIAN_DEFAULT_PROFILES: the pin after '@' in ${item}@… is not sha256:<64 hex digits>."
            error "  Write the entry as  ${item}@sha256:<digest>  or without a pin."
            return 1
        fi
    fi
    if [[ "${item}" != https://* ]]; then
        error "GENTIAN_DEFAULT_PROFILES names ${item}, which is not an https address."
        error "  A default profile is fetched from a catalog and held to a digest; a local file"
        error "  or an http address has neither a catalog to record nor an index to state one."
        error "  What to do, one of:"
        error "    - remove the line from install.env: unset, step 0 places the extra services"
        error "      from the store's catalog, at the digests the catalog's index lists;"
        error "    - publish the file in a catalog (https, profiles/<name>.yaml beside index.yaml)"
        error "      and name that address, with @sha256:<digest> to pin the build;"
        error "    - set GENTIAN_DEFAULT_PROFILES= (empty) and install the profile through the"
        error "      director once the cluster is up:  kubectl gentian apps install"
        return 1
    fi
    # In a variable: bash 3.2 reads a pattern with brackets and parentheses
    # reliably only from one.
    local address='^(https://[^[:space:]?#@]+)/profiles/([a-z0-9]([a-z0-9-]*[a-z0-9])?)\.yaml$'
    if [[ ! "${item}" =~ ${address} ]]; then
        error "GENTIAN_DEFAULT_PROFILES names ${item}, which is not a profile in a catalogue."
        error "  The address is <catalogue>/profiles/<name>.yaml: the index is read from"
        error "  <catalogue>/index.yaml, and the file's digest from the entry it lists as <name>."
        return 1
    fi
    _DP_URL="${item}"; _DP_BASE="${BASH_REMATCH[1]}"; _DP_NAME="${BASH_REMATCH[2]}"
    for reserved in ${_PLATFORM_PROFILE_NAMES}; do
        if [[ "${_DP_NAME}" == "${reserved}" ]]; then
            error "GENTIAN_DEFAULT_PROFILES names the profile ${_DP_NAME}, which the platform ships itself;"
            error "  a catalog entry cannot take its name. Remove the entry."
            return 1
        fi
    done
    return 0
}

# What this run placed, for the commit that records it (bootstrap.sh).
_GENTIAN_DEFAULT_PROFILE_NOTES=""

# _default_profile_place <cluster> <catalogue dir> <entry> -- one entry of the
# list: read the index, settle the digest, fetch, check, write.
#
#   returns 0   placed, already there, kept as it is, or skipped because the
#               catalog cannot be reached (a warning)
#   returns 1   everything else, and nothing of the profile was written
_default_profile_place() {
    local cluster="$1" dir="$2" entry="$3"
    local name base url pin listed="" want how index body rc recorded have have_origin origin refusal size unrecorded="" superseded=""

    _default_profile_parse "${entry}" || return 1
    name="${_DP_NAME}"; base="${_DP_BASE}"; url="${_DP_URL}"; pin="${_DP_PIN}"

    # The index first: it is where the catalog says which build an entry is.
    index="$(mktemp)"
    rc=0; _catalogue_fetch "${base}/index.yaml" "${index}" || rc=$?
    if [[ "${rc}" == "2" ]]; then
        rm -f "${index}"
        if [[ -f "${dir}/${name}.yaml" ]]; then
            warn "The catalogue ${base} cannot be reached; default profile ${name} stays as the cluster's definition has it."
        else
            warn "The catalogue ${base} cannot be reached; default profile ${name} is not placed."
            warn "  The install goes on without it. Run the installer again when the catalog answers,"
            warn "  or install it through the director:  kubectl gentian apps install"
        fi
        return 0
    fi
    if [[ "${rc}" == "0" ]]; then listed="$(_catalogue_index_digest "${index}" "${name}")"; fi
    rm -f "${index}"

    if [[ -n "${pin}" ]]; then
        want="${pin}"; how="pinned in GENTIAN_DEFAULT_PROFILES"
        if [[ -n "${listed}" && "${listed}" != "${pin}" ]]; then
            warn "The index of ${base} lists ${name} at ${listed}; GENTIAN_DEFAULT_PROFILES pins ${pin}."
            warn "  The pin is what is required: the index does not override it."
        fi
    elif [[ -n "${listed}" ]]; then
        want="${listed}"; how="listed by the index of ${base}"
    elif [[ "${rc}" == "0" && -z "${GENTIAN_DEFAULT_PROFILES+set}" ]] && _default_profile_is_extra_service "${entry}"; then
        # One of the extra services the store's catalog does not list (yet):
        # the default list names what a store offers, and a store that offers
        # less is not a reason to stop an install.
        if [[ -f "${dir}/${name}.yaml" ]]; then
            warn "The index of ${base} lists no ${name}; default profile ${name} stays as the cluster's definition has it."
        else
            warn "The index of ${base} lists no ${name}; that extra service is not placed. The install goes on."
        fi
        return 0
    else
        error "Default profile ${name}: there is no digest to hold it to."
        if [[ "${rc}" == "0" ]]; then
            error "  ${base}/index.yaml answers and lists no entry named ${name} with a sha256 digest."
        else
            error "  ${base}/index.yaml answered ${_CATALOGUE_FETCH_CODE}: the catalogue publishes no index there."
        fi
        error "  Nothing was written. A profile is placed only at a digest somebody stated:"
        error "    - pin it yourself:  GENTIAN_DEFAULT_PROFILES=${url}@sha256:<digest>"
        error "    - or leave it out:  GENTIAN_DEFAULT_PROFILES=  (empty), or --disable-api-extensions"
        return 1
    fi

    # What the cluster's definition already holds under that name. A build
    # that differs from the one wanted is somebody's, or an earlier decision,
    # and is kept and said -- except an extra service this installer placed
    # from the store's catalog and nobody pinned: that follows the
    # catalog, and is replaced by the build its index lists.
    body="$(mktemp)"
    if [[ -f "${dir}/${name}.yaml" ]]; then
        recorded="$(_profile_bundle_recorded "${dir}" "${name}")"
        if [[ -n "${recorded}" ]]; then
            have="${recorded%% *}"; have_origin="${recorded#* }"
            if [[ "${have}" != "${want}" && -z "${pin}" && -n "${listed}" ]] \
                && _default_profile_is_extra_service "${entry}" \
                && [[ "${base}" == "$(_store_catalogue_base)" ]] \
                && _is_store_catalogue_origin "${cluster}" "${have_origin}"; then
                superseded="${have}"
                recorded=""
            fi
        fi
        if [[ -n "${recorded}" ]]; then
            rm -f "${body}"
            if [[ "${have}" == "${want}" ]]; then
                _kustomization_list "${dir}/kustomization.yaml" resource "${name}.yaml" || return 1
                _kustomization_list "${dir}/kustomization.yaml" patch "${name}.bundle.yaml" || return 1
                info "  default profile ${name} is in the cluster's definition at ${want:0:19} (${have_origin}); unchanged"
                return 0
            fi
            warn "Default profile ${name}: the cluster's definition holds another build than the one ${how}."
            warn "    recorded:  ${have} (${have_origin})"
            warn "    wanted:    ${want}"
            warn "  It is kept as it is; nothing was fetched and nothing was replaced. To move to the"
            warn "  wanted build, install it through the director (kubectl gentian apps install, with"
            warn "  that digest), or remove clusters/${cluster}/catalogue/${name}.yaml and ${name}.bundle.yaml"
            warn "  and their two lines in kustomization.yaml, commit, and run the installer again."
            return 0
        fi
        # Placed by an installer that recorded nothing, which wrote whatever
        # the address served on every run. When its bytes are the build
        # wanted, the record is added and the profile is not touched. When
        # they are not, it is replaced as it always was -- now by a file held
        # to the digest, and said: nobody decided on a copy nothing recorded.
        if [[ -z "${superseded}" ]]; then
            have="sha256:$(sha256_of "${dir}/${name}.yaml")"
            if [[ "${have}" == "${want}" ]]; then
                cat "${dir}/${name}.yaml" > "${body}"
            else
                unrecorded="${have}"
            fi
        fi
    fi
    if [[ ! -s "${body}" ]]; then
        rc=0; _catalogue_fetch "${url}" "${body}" || rc=$?
        if [[ "${rc}" == "2" ]]; then
            rm -f "${body}"
            warn "The catalogue ${base} stopped answering; default profile ${name} is not placed${unrecorded:+, and the copy in the definition stays}."
            warn "  The install goes on without it. Run the installer again when the catalog answers."
            return 0
        fi
        if [[ "${rc}" != "0" ]]; then
            rm -f "${body}"
            error "Default profile ${name}: ${url} answered ${_CATALOGUE_FETCH_CODE}."
            error "  The catalog answers and does not serve the file. Nothing was written."
            error "  Check the address, or leave the profile out: GENTIAN_DEFAULT_PROFILES= (empty)."
            return 1
        fi
        have="sha256:$(sha256_of "${body}")"
        if [[ "${have}" != "${want}" ]]; then
            rm -f "${body}"
            error "Default profile ${name}: what ${url} serves is not the build ${how}."
            error "    wanted:  ${want}"
            error "  The bytes do not hash to it, so nothing was written and the install stops here:"
            error "  a catalog that serves another build than the one stated is either out of step"
            error "  with its own index or not the catalog it says it is."
            if [[ -n "${pin}" ]]; then
                error "  If the catalog published a new build on purpose, check it and move the pin."
            else
                error "  Run the installer again in a few minutes if the catalog was being published;"
                error "  if it still disagrees with itself, tell whoever runs it."
            fi
            error "  To install without the profile: GENTIAN_DEFAULT_PROFILES= (empty), or --disable-api-extensions."
            return 1
        fi
    fi

    size="$(wc -c < "${body}" | tr -d '[:space:]')"
    if [[ "${size}" -gt "${_PROFILE_BUNDLE_MAX_BYTES}" ]]; then
        rm -f "${body}"
        error "Default profile ${name} is ${size} bytes; a bundle of more than ${_PROFILE_BUNDLE_MAX_BYTES} cannot be carried"
        error "  beside its profile for the operator to check. Nothing was written."
        return 1
    fi
    refusal="$(_profile_bundle_refusal "${body}" "${name}")"
    if [[ -n "${refusal}" ]]; then
        rm -f "${body}"
        error "Default profile ${name}: the file is the build ${how}, and is not a bundle that is placed:"
        error "  ${refusal}."
        error "  Nothing was written. This is the catalog's to correct."
        return 1
    fi

    origin="$(_default_profile_origin "${base}" "${cluster}")"
    if ! gentian_materialise_profile "${dir}" "${name}" "${body}" "${origin}"; then
        rm -f "${body}"
        error "Default profile ${name} could not be written to ${dir}."
        return 1
    fi
    rm -f "${body}"
    _GENTIAN_DEFAULT_PROFILE_NOTES+="Default profile ${name}: ${want}, ${how}.
  Fetched from ${url}; recorded as ${origin}.
"
    if [[ -n "${superseded}" ]]; then
        _GENTIAN_DEFAULT_PROFILE_NOTES+="  Replaces ${superseded}: an extra service follows the catalogue of the store.
"
    fi
    if [[ -n "${superseded}" ]]; then
        info "  default profile ${name} follows the store's catalogue: ${superseded:0:19} replaced by ${want:0:19}, ${how}"
        return 0
    fi
    info "  default profile ${name} at ${want:0:19} (${how}), recorded as ${origin}"
    if [[ -n "${unrecorded}" ]]; then
        warn "  It replaces the copy an earlier install wrote with no digest (${unrecorded})."
    fi
    return 0
}

# preview_default_profiles <cluster> -- what an install would do about the
# default profiles, for --dry-run and --validate. It reads the cluster's
# definition and asks no catalog.
preview_default_profiles() {
    local cluster="$1" dir list entry recorded
    local -a entries=()
    dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/catalogue"
    if [[ "${GENTIAN_DISABLE_API_EXTENSIONS:-0}" == "1" ]]; then
        info "Default profiles: none would be placed (--disable-api-extensions)."
        return 0
    fi
    if [[ "$(gentian_extra_services_enabled "${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel/claims/cluster.yaml")" != "true" ]]; then
        info "Default profiles: the extra services (${_EXTRA_SERVICE_PROFILES// /, }) would not be placed (--no-extra-services)."
    fi
    list="$(_default_profiles_configured "${cluster}")"
    [[ -n "${list}" ]] || { info "Default profiles: none would be placed."; return 0; }
    IFS=',' read -r -a entries <<< "${list}"
    for entry in ${entries[@]+"${entries[@]}"}; do
        entry="$(printf '%s' "${entry}" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        [[ -n "${entry}" ]] || continue
        if ! _default_profile_parse "${entry}"; then
            gentian_would "stop at step 0 for that reason; this preview goes on"
            continue
        fi
        if [[ -f "${dir}/${_DP_NAME}.yaml" ]]; then
            recorded="$(_profile_bundle_recorded "${dir}" "${_DP_NAME}")"
            info "Default profile ${_DP_NAME}: the cluster's definition holds it (${recorded:-no digest recorded})."
        fi
        if [[ -n "${_DP_PIN}" ]]; then
            gentian_would "read ${_DP_BASE}/index.yaml and place ${_DP_NAME} only if ${_DP_URL} hashes to the pinned ${_DP_PIN}"
        else
            gentian_would "read ${_DP_BASE}/index.yaml and place ${_DP_NAME} only if ${_DP_URL} hashes to the digest it lists"
        fi
    done
    return 0
}

# =============================================================================
# The store's catalogs (store API 1.5, docs/design/store-contract.md §9).
#
# A store lists in GET /v1/meta the catalog sources its entries come from.
# An app bought there is installed from the catalog its coordinate names,
# and the director refuses a catalog the cluster declares no source for. So
# on every run step 0 reads the list of the store the claim names and adds to
# spec.catalogue.sources each catalog of a name the claim does not have,
# marked addedBy: store. It adds and does nothing else: a source the claim
# has is never changed or removed, whoever added it; a name it holds at
# another address is left and said; a name a tenant uses for a catalog of
# its own is not added, as the director would refuse it; only https is
# added; a store that cannot be reached, or lists none, is a warning. A
# catalog the store stops listing stays on the claim.
#
# This takes the store's word for where its catalogs are. Every bundle
# fetched from one is still held to the digest of its install.
# =============================================================================

# _store_catalogue_name_ok <name> -- a catalog's name: lower-case letters,
# digits and hyphens, the first half of a coordinate.
_store_catalogue_name_ok() {
    local pattern='^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
    [[ "$1" =~ ${pattern} ]]
}

# _store_catalogue_url_ok <url> -- an https address the claim can carry as a
# plain scalar: what the director accepts of `catalogues add`
# (gitops.checkCatalogueSource).
_store_catalogue_url_ok() {
    local pattern='^https://[^[:space:]#"'"'"']+$'
    [[ "$1" =~ ${pattern} && ${#1} -le 2048 ]]
}

# _claim_sources_tsv <claim file> -- "<name>\t<url>\t<oci artifact>\t<addedBy>"
# for every source the claim lists, as written.
_claim_sources_tsv() {
    _yaml_docs_json "$1" 2>/dev/null | jq -r '
        select(type == "object") | (.spec.catalogue.sources // []) | .[]? | select(type == "object")
        | [(.name // "" | tostring), (.url // "" | tostring), (.oci.artifact // "" | tostring), (.addedBy // "" | tostring)]
        | @tsv' 2>/dev/null || true
}

# _tenant_catalogue_holders <tenants dir> <name> -- the tenants, comma
# separated, that have a catalog of their own of this name.
_tenant_catalogue_holders() {
    local dir="$1" name="$2" file out=""
    [[ -d "${dir}" ]] || return 0
    for file in "${dir}"/*/tenant.yaml; do
        [[ -f "${file}" ]] || continue
        if _yaml_docs_json "${file}" 2>/dev/null | jq -e --arg n "${name}" \
            'select(type == "object") | (.spec.catalogue.sources // []) | any(.[]?; (.name // "") == $n)' >/dev/null 2>&1; then
            out+="${out:+, }$(basename "$(dirname "${file}")")"
        fi
    done
    printf '%s' "${out}"
}

# _claim_append_sources <claim file> <entries file> <store> -- add the
# entries ("<name>\t<url>" lines) to spec.catalogue.sources, each marked
# addedBy: store, after the last source the list has; or as the list, when
# the claim has none or `sources: []`. A line editor: nothing else of the
# claim moves, its comments included. Fails, writing nothing, on a catalog
# section written on one line.
_claim_append_sources() {
    local claim="$1" listed_file="$2" store="$3" tmp rc=0
    tmp="$(mktemp)"
    awk -v store="${store}" -v entries="${listed_file}" '
        function emit(ind,    line, f, name) {
            while ((getline line < entries) > 0) {
                split(line, f, "\t"); name = f[1]
                if (name ~ /^[0-9]+$/ || name ~ /^(true|false|null|yes|no|on|off|y|n)$/) name = "\"" name "\""
                print ind "- name: " name
                print ind "  # Listed by the store " store " (GET /v1/meta); added by the installer."
                print ind "  url: " f[2]
                print ind "  addedBy: store"
            }
            close(entries)
        }
        function content(t) { return t !~ /^[ \t]*$/ && t !~ /^[ \t]*#/ }
        function indent(t) { match(t, /^ */); return RLENGTH }
        { L[NR] = $0 }
        END {
            n = NR
            for (i = 1; i <= n; i++) if (L[i] ~ /^spec:[ \t]*$/) { spec = i; break }
            if (!spec) exit 3
            for (i = spec + 1; i <= n; i++) {
                if (content(L[i]) && indent(L[i]) == 0) break
                if (L[i] ~ /^  catalogue:[ \t]*(#.*)?$/) { cat = i; break }
            }
            if (!cat) exit 4
            catend = n + 1
            for (i = cat + 1; i <= n; i++) if (content(L[i]) && indent(L[i]) <= 2) { catend = i; break }
            for (i = cat + 1; i < catend; i++) if (L[i] ~ /^    sources:/) { src = i; break }
            if (src) {
                v = L[src]; sub(/^    sources:[ \t]*/, "", v); sub(/[ \t]*#.*$/, "", v)
                if (v == "[]") {
                    for (i = 1; i < src; i++) print L[i]
                    print "    sources:"; emit("      ")
                    for (i = src + 1; i <= n; i++) print L[i]
                    exit 0
                }
                if (v != "") exit 5
                ins = src; ind = ""
                for (i = src + 1; i < catend; i++) {
                    if (!content(L[i])) continue
                    d = indent(L[i]); t = substr(L[i], d + 1)
                    if (d < 4 || (d == 4 && t !~ /^- /)) break
                    if (ind == "" && t ~ /^- /) ind = substr(L[i], 1, d)
                    ins = i
                }
                if (ind == "") ind = "      "
                for (i = 1; i <= ins; i++) print L[i]
                emit(ind)
                for (i = ins + 1; i <= n; i++) print L[i]
                exit 0
            }
            last = cat
            for (i = cat + 1; i < catend; i++) if (content(L[i])) last = i
            for (i = 1; i <= last; i++) print L[i]
            print "    sources:"; emit("      ")
            for (i = last + 1; i <= n; i++) print L[i]
        }' "${claim}" > "${tmp}" || rc=$?
    if [[ "${rc}" != "0" ]]; then
        rm -f "${tmp}"
        return 1
    fi
    cat "${tmp}" > "${claim}"
    rm -f "${tmp}"
}

# What this run added, for the commit that records it (bootstrap.sh).
_GENTIAN_STORE_CATALOGUE_NOTES=""

# ensure_claim_store_catalogues <claim file> <tenants dir> -- add to the claim
# the catalogs its store lists and it has no source for (above).
#
# Never fails the install: whatever keeps a catalog from being added is a
# warning, and the claim is then as it was.
ensure_claim_store_catalogues() {
    local claim="$1" tenants="$2" store meta rc=0 have name url row holders
    local entries added="" present=0 listed=0
    [[ -f "${claim}" ]] || return 0
    store="$(yq_get '.spec.catalogue.storeUrl' "${claim}" 2>/dev/null || true)"
    store="$(printf '%s' "${store}" | tr -d '[:space:]')"
    store="${store%/}"
    [[ -n "${store}" ]] || return 0
    if ! _store_catalogue_url_ok "${store}"; then
        warn "claims/cluster.yaml names the store ${store}, which is not an https address;"
        warn "  the catalogs it lists are not read."
        return 0
    fi
    if [[ "$(gentian_usage_statistics_enabled)" != "true" ]]; then
        info "The store's catalogs are not read: usage statistics are off, so the App Store is not offered."
        return 0
    fi

    meta="$(mktemp)"
    _catalogue_fetch "${store}/v1/meta" "${meta}" || rc=$?
    if [[ "${rc}" != "0" ]]; then
        rm -f "${meta}"
        if [[ "${rc}" == "2" ]]; then
            warn "The store ${store} cannot be reached; the catalogues it lists are not checked this run."
        else
            warn "The store ${store} answered ${_CATALOGUE_FETCH_CODE} to GET /v1/meta; the catalogues it lists are not checked this run."
        fi
        warn "  The install goes on. An app bought there installs only from a catalog the cluster"
        warn "  has; add one by hand with:  kubectl gentian catalogues add <name> <https address>"
        return 0
    fi
    if ! jq -e 'type == "object" and ((.catalogues // null) | type == "array")' "${meta}" >/dev/null 2>&1; then
        rm -f "${meta}"
        warn "The store ${store} lists no catalogues in GET /v1/meta (store API 1.5); none were added."
        warn "  An app bought there installs only from a catalog the cluster has; add one by hand with:"
        warn "    kubectl gentian catalogues add <name> <https address>"
        return 0
    fi

    have="$(_claim_sources_tsv "${claim}")"
    entries="$(mktemp)"
    while IFS=$'\t' read -r name url; do
        listed=$((listed + 1))
        url="${url%/}"
        if ! _store_catalogue_name_ok "${name}" || ! _store_catalogue_url_ok "${url}"; then
            warn "The store ${store} lists a catalogue the cluster cannot take: name '${name}', address '${url}'."
            warn "  A name is lower-case letters, digits and hyphens; an address is https. It was not added."
            continue
        fi
        row="$(awk -F'\t' -v n="${name}" '$1 == n { print; exit }' <<< "${have}")"
        if [[ -n "${row}" ]]; then
            local own_url own_oci
            own_url="$(cut -f2 <<< "${row}")"; own_oci="$(cut -f3 <<< "${row}")"
            if [[ -z "${own_oci}" && "${own_url%/}" == "${url}" ]]; then
                present=$((present + 1))
                continue
            fi
            warn "The store ${store} lists the catalogue ${name} at ${url};"
            warn "  claims/cluster.yaml names ${name} at ${own_url:-oci: ${own_oci}}. The claim's is kept."
            warn "  An app bought there is installed from the claim's ${name}. To take the store's:"
            warn "    kubectl gentian catalogues remove ${name} && kubectl gentian catalogues add ${name} ${url}"
            continue
        fi
        holders="$(_tenant_catalogue_holders "${tenants}" "${name}")"
        if [[ -n "${holders}" ]]; then
            warn "The store ${store} lists the catalogue ${name}, and tenant ${holders} has a catalogue of"
            warn "  its own of that name; the store's is not added for every tenant, as the name would then"
            warn "  mean two things there. Each other tenant installs nothing bought from ${name}."
            continue
        fi
        if grep -q "^${name}"$'\t' "${entries}"; then
            continue
        fi
        printf '%s\t%s\n' "${name}" "${url}" >> "${entries}"
        added+="${added:+, }${name} (${url})"
        have+=$'\n'"${name}"$'\t'"${url}"$'\t\t'"store"
    done < <(jq -r '.catalogues[]? | select(type == "object") | [(.name // "" | tostring), (.url // "" | tostring)] | @tsv' "${meta}" 2>/dev/null)
    rm -f "${meta}"

    if [[ -z "${added}" ]]; then
        rm -f "${entries}"
        if (( listed > 0 && present == listed )); then
            info "  The store's catalogues are all on the claim (${listed})."
        fi
        return 0
    fi

    local before after copy
    before="$(_claim_sources_tsv "${claim}" | wc -l | tr -d '[:space:]')"
    copy="$(mktemp)"; cat "${claim}" > "${copy}"
    if ! _claim_append_sources "${claim}" "${entries}" "${store}"; then
        rm -f "${entries}" "${copy}"
        warn "claims/cluster.yaml: spec.catalogue is not written as a block, so the store's catalogs"
        warn "  ${added} were not added. Write the section as a block, or add them by hand."
        return 0
    fi
    after="$(_claim_sources_tsv "${claim}")"
    while IFS=$'\t' read -r name url; do
        if ! grep -qxF "${name}"$'\t'"${url}"$'\t\t'"store" <<< "${after}"; then
            after=""
            break
        fi
    done < "${entries}"
    if [[ -z "${after}" || "$(wc -l <<< "${after}" | tr -d '[:space:]')" != "$(( before + $(wc -l < "${entries}") ))" ]]; then
        cat "${copy}" > "${claim}"
        rm -f "${entries}" "${copy}"
        warn "claims/cluster.yaml did not read back as written once the store's catalogs were added;"
        warn "  it is left as it was. Add them by hand: ${added}."
        return 0
    fi
    rm -f "${entries}" "${copy}"
    _GENTIAN_STORE_CATALOGUE_NOTES+="Catalogues listed by the store ${store} (GET /v1/meta), added for every
tenant with addedBy: store: ${added}.
"
    info "claims/cluster.yaml: added the catalogues the store ${store} lists: ${added}."
    info "  Marked addedBy: store. Each bundle is still held to the digest of its install; a"
    info "  catalog the store stops listing stays until it is removed (kubectl gentian catalogues remove)."
}

# preview_claim_store_catalogues <claim file> <tenants dir> -- what
# ensure_claim_store_catalogues would do, for --dry-run and --validate: the
# same code on a copy of the claim. It reads the store, and writes nothing.
preview_claim_store_catalogues() {
    local claim="$1" tenants="$2" copy said
    [[ -f "${claim}" ]] || return 0
    copy="$(mktemp)"
    cat "${claim}" > "${copy}"
    said="$(ensure_claim_store_catalogues "${copy}" "${tenants}" 2>&1 || true)"
    said="${said//${copy}/${claim}}"
    if ! cmp -s "${claim}" "${copy}"; then
        gentian_would "add the store's catalogues to claims/cluster.yaml, then commit and push the edit signed"
        printf '%s\n' "${said}" | sed 's/^/    /'
    elif [[ -n "${said}" ]]; then
        printf '%s\n' "${said}"
    fi
    rm -f "${copy}"
}
