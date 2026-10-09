#!/usr/bin/env bash
# =============================================================================
# scripts/lib/catalogue.sh — App catalogue sync, operator Helm bootstrap, and orchestrator handoff.
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
    if cmp -s "${repo}" "${installed}"; then
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
# A profile reaches a cluster through the director, fetched from a catalogue
# and held to a digest. One arrives earlier than any director: the Operations
# Console, which step 0 places in clusters/<cluster>/catalogue/ so that the
# cluster comes up with it. This is that path, and it is held to the same
# rule: the bytes are refused unless they hash to a digest somebody stated --
# the catalogue's own index, or the person installing, who may pin one -- and
# what is written is what the director writes for an install of the same
# bundle, byte for byte (internal/director/gitops/catalogue.go,
# MaterialiseProfile): the bundle as served, the patch that puts those bytes
# and the catalogue's origin on the profile, and both in the kustomization.
# A test holds the two to each other (installer_profiles_test.go).
# =============================================================================

# The largest bundle that can be carried beside its profile
# (internal/profilebundle, MaxBytes).
_PROFILE_BUNDLE_MAX_BYTES=184320

# The profiles the platform's own chart ships. A catalogue entry of one of
# these names is refused by the director (gitops.PlatformProfile), and here.
_PLATFORM_PROFILE_NAMES="admin-console app-store concierge desktop"

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
#   2  the catalogue cannot be reached: no answer, or a 5xx
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
# What the director asks of a bundle from a catalogue of the whole cluster
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
# Writes one verified bundle into the cluster's catalogue directory as the
# director's MaterialiseProfile does: <name>.yaml, the bytes unchanged;
# <name>.bundle.yaml, the patch that carries them and the origin on the
# profile; and both listed in kustomization.yaml. The caller has checked the
# digest and the bundle; nothing here fetches or decides.
gentian_materialise_profile() {
    local dir="$1" name="$2" body="$3" origin="$4" head api ns tmp
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

    _kustomization_list "${dir}/kustomization.yaml" resource "${name}.yaml" || return 1
    _kustomization_list "${dir}/kustomization.yaml" patch "${name}.bundle.yaml" || return 1
}

# _default_profile_origin <catalogue address> <cluster> -- the origin recorded
# for a profile fetched from this address: "cluster/<source>".
#
# The name of the catalogue the Cluster claim declares at that address, when
# it declares one -- then the record is the one an install through the
# director from that catalogue leaves. Otherwise a name made of the address
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
        name="$(printf '%s' "${base#https://}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' | sed 's/--*/-/g; s/^-//; s/-$//')"
    fi
    printf 'cluster/%s' "${name}"
}

# _default_profiles_configured -- the list, as GENTIAN_DEFAULT_PROFILES gives
# it or, unset, the Operations Console at the store's catalogue.
_default_profiles_configured() {
    printf '%s' "${GENTIAN_DEFAULT_PROFILES-${GENTIAN_STORE_CATALOGUE_URL:-https://catalogue.aluvian.io}/profiles/operations-console.yaml}"
}

# _default_profile_parse <entry> -- reads one entry of the list into
# _DP_URL, _DP_BASE, _DP_NAME and _DP_PIN, or says why it is not one.
#
# An entry is the https address of a profile in a catalogue,
# <catalogue>/profiles/<name>.yaml, optionally followed by @sha256:<digest>.
# A local file was accepted once and is not any more: it has no catalogue to
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
        error "  A default profile is fetched from a catalogue and held to a digest; a local file"
        error "  or an http address has neither a catalogue to record nor an index to state one."
        error "  What to do, one of:"
        error "    - remove the line from install.env: unset, step 0 places the Operations Console"
        error "      from the catalogue, at the digest the catalogue's index lists;"
        error "    - publish the file in a catalogue (https, profiles/<name>.yaml beside index.yaml)"
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
            error "  a catalogue entry cannot take its name. Remove the entry."
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
#               catalogue cannot be reached (a warning)
#   returns 1   everything else, and nothing of the profile was written
_default_profile_place() {
    local cluster="$1" dir="$2" entry="$3"
    local name base url pin listed="" want how index body rc recorded have have_origin origin refusal size unrecorded=""

    _default_profile_parse "${entry}" || return 1
    name="${_DP_NAME}"; base="${_DP_BASE}"; url="${_DP_URL}"; pin="${_DP_PIN}"

    # The index first: it is where the catalogue says which build an entry is.
    index="$(mktemp)"
    rc=0; _catalogue_fetch "${base}/index.yaml" "${index}" || rc=$?
    if [[ "${rc}" == "2" ]]; then
        rm -f "${index}"
        if [[ -f "${dir}/${name}.yaml" ]]; then
            warn "The catalogue ${base} cannot be reached; default profile ${name} stays as the cluster's definition has it."
        else
            warn "The catalogue ${base} cannot be reached; default profile ${name} is not placed."
            warn "  The install goes on without it. Run the installer again when the catalogue answers,"
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

    # What the cluster's definition already holds under that name. It is
    # never replaced here: a build that differs from the one wanted is
    # somebody's, or an earlier decision, and is kept and said.
    body="$(mktemp)"
    if [[ -f "${dir}/${name}.yaml" ]]; then
        recorded="$(_profile_bundle_recorded "${dir}" "${name}")"
        if [[ -n "${recorded}" ]]; then
            have="${recorded%% *}"; have_origin="${recorded#* }"
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
        have="sha256:$(sha256_of "${dir}/${name}.yaml")"
        if [[ "${have}" == "${want}" ]]; then
            cat "${dir}/${name}.yaml" > "${body}"
        else
            unrecorded="${have}"
        fi
    fi
    if [[ ! -s "${body}" ]]; then
        rc=0; _catalogue_fetch "${url}" "${body}" || rc=$?
        if [[ "${rc}" == "2" ]]; then
            rm -f "${body}"
            warn "The catalogue ${base} stopped answering; default profile ${name} is not placed${unrecorded:+, and the copy in the definition stays}."
            warn "  The install goes on without it. Run the installer again when the catalogue answers."
            return 0
        fi
        if [[ "${rc}" != "0" ]]; then
            rm -f "${body}"
            error "Default profile ${name}: ${url} answered ${_CATALOGUE_FETCH_CODE}."
            error "  The catalogue answers and does not serve the file. Nothing was written."
            error "  Check the address, or leave the profile out: GENTIAN_DEFAULT_PROFILES= (empty)."
            return 1
        fi
        have="sha256:$(sha256_of "${body}")"
        if [[ "${have}" != "${want}" ]]; then
            rm -f "${body}"
            error "Default profile ${name}: what ${url} serves is not the build ${how}."
            error "    wanted:  ${want}"
            error "  The bytes do not hash to it, so nothing was written and the install stops here:"
            error "  a catalogue that serves another build than the one stated is either out of step"
            error "  with its own index or not the catalogue it says it is."
            if [[ -n "${pin}" ]]; then
                error "  If the catalogue published a new build on purpose, check it and move the pin."
            else
                error "  Run the installer again in a few minutes if the catalogue was being published;"
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
        error "  Nothing was written. This is the catalogue's to correct."
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
    info "  default profile ${name} at ${want:0:19} (${how}), recorded as ${origin}"
    if [[ -n "${unrecorded}" ]]; then
        warn "  It replaces the copy an earlier install wrote with no digest (${unrecorded})."
    fi
    return 0
}

# preview_default_profiles <cluster> -- what an install would do about the
# default profiles, for --dry-run and --validate. It reads the cluster's
# definition and asks no catalogue.
preview_default_profiles() {
    local cluster="$1" dir list entry recorded
    local -a entries=()
    dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/catalogue"
    if [[ "${GENTIAN_DISABLE_API_EXTENSIONS:-0}" == "1" ]]; then
        info "Default profiles: none would be placed (--disable-api-extensions)."
        return 0
    fi
    list="$(_default_profiles_configured)"
    [[ -n "${list}" ]] || { info "Default profiles: none would be placed (GENTIAN_DEFAULT_PROFILES is empty)."; return 0; }
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
