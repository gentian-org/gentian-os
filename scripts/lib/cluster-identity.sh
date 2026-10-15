#!/usr/bin/env bash
# =============================================================================
# scripts/lib/cluster-identity.sh — one run, one cluster.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
#
# A run is given three things that each name a cluster: the settings
# (install.env and whatever the shell exports), the deployments checkout, and
# the cluster kubectl reaches. Nothing used to ask whether they name the same
# one. On a machine that runs two clusters they did not, three times in one
# morning:
#
#   - a shell still exported a second cluster's KUBECONFIG, and a run meant
#     for the first went to the second;
#   - a run from the second cluster's checkout reached the first cluster,
#     replaced the salt it brought with the first cluster's, and wrote that
#     salt into the second cluster's credential cache;
#   - a shell exported a second cluster's GENTIAN_DEPLOYMENTS_PATH, and a run
#     from the first cluster's checkout found no definition of it there and
#     began writing one into the second cluster's repository.
#
# So before anything that belongs to a cluster is read from it, cached,
# written, committed or applied, the run establishes that the three agree:
#
#   A  the checkout is the repository and branch the settings name, and a
#      repository that already defines other clusters is not given a new one
#      without being asked (gentian_check_deployments_checkout, then
#      gentian_check_one_cluster scaffold);
#   B  the cluster kubectl reaches is the one the definition describes: the
#      Cluster claim in git against the Cluster object on the cluster, by
#      name and kernel domain (gentian_check_one_cluster);
#   C  a derivation salt is never taken from, nor written into, another
#      cluster's credential cache (credentials.sh, which asks
#      gentian_cluster_identity_verified);
#   D  the settings a shell exports over install.env are named at the start,
#      so a terminal carrying another cluster's is seen at once
#      (gentian_report_identity_exports);
#   E  ~/.gentian/config is written only once A and B have passed
#      (save_kubectl_gentian_config, common.sh).
#
# --force is not a way past any of this: it re-applies steps whose check() is
# satisfied, and says nothing about which cluster they belong to. The two
# confirmations below each name the thing they confirm, so a value left
# exported cannot wave through a different cluster or repository.
#
#   GENTIAN_ADD_CLUSTER=<cluster id>      write a new cluster into a
#                                         repository that defines others
#   GENTIAN_ACCEPT_CLUSTER=<claim name>   the cluster that answered is the one
#                                         the definition describes, although
#                                         the two disagree (a changed domain)
# =============================================================================

# The settings that say which cluster a run is for. An export of any of them
# beats install.env (load_env_file), which is right for one run and wrong for
# every run after it in the same terminal.
GENTIAN_IDENTITY_VARS=(
    KUBECONFIG
    GENTIAN_DEPLOYMENTS_PATH
    GENTIAN_DEPLOYMENTS_REPO
    GENTIAN_DEPLOYMENTS_BRANCH
    GENTIAN_DEPLOYMENTS_CLUSTER_ID
    GENTIAN_DEPLOYMENTS_STAGE
    GENTIAN_CREDENTIAL_CACHE
    KERNEL_DOMAIN
    DERIVATION_SALT
    MASTER_PASSWORD
)

# Which of them this run took from the shell, by name. Set by
# gentian_report_identity_exports before anything is loaded.
_GENTIAN_ENV_IDENTITY_VARS=""

# What A and B concluded, for the current settings.
#   _GENTIAN_CHECKOUT_CHECKED   path|repo|branch the checkout was matched for
#   _GENTIAN_ONE_CLUSTER_KEY    path|id|context the cluster was matched for
#   _GENTIAN_CLUSTER_IDENTITY   "verified": the cluster's Cluster object is the
#                               definition's; "unasked": there was nothing to
#                               compare (no claim yet, or no Cluster object)
_GENTIAN_CHECKOUT_CHECKED=""
_GENTIAN_ONE_CLUSTER_KEY=""
_GENTIAN_CLUSTER_IDENTITY=""

# =============================================================================
# D — what this shell brings
# =============================================================================

# gentian_report_identity_exports — name the cluster settings exported into
# this run. Names only: two of them are credentials. Called once, first, so
# the warning is at the top of the output and not below a page of steps.
gentian_report_identity_exports() {
    local v flags found="" in_file="" f="${INSTALL_CONFIG_FILE:-}"
    _GENTIAN_ENV_IDENTITY_VARS=""
    for v in "${GENTIAN_IDENTITY_VARS[@]}"; do
        # Exported with a value: what load_env_file keeps over the file.
        # declare -p rather than `compgen -e | grep -q`, which SIGPIPEs the
        # builtin under pipefail and then reports a match as none.
        [[ -n "${!v:-}" ]] || continue
        flags="$(declare -p "${v}" 2>/dev/null || true)"
        flags="${flags#declare -}"
        flags="${flags%% *}"
        [[ "${flags}" == *x* ]] || continue
        found+=" ${v}"
        _GENTIAN_ENV_IDENTITY_VARS+=" ${v}"
        if [[ "${INSTALL_AUTO_LOAD_CONFIG:-1}" == "1" && -n "${f}" && -r "${f}" ]] &&
           grep -qE "^[[:space:]]*(export[[:space:]]+)?${v}=" "${f}"; then
            in_file+=" ${v}"
        fi
    done
    _GENTIAN_ENV_IDENTITY_VARS+=" "
    found="${found# }"; in_file="${in_file# }"
    [[ -n "${found}" ]] || return 0
    echo ""
    warn "This shell exports settings that decide which cluster this run is for:"
    warn "  ${found}"
    warn "  They are used in place of ${f:-install.env}."
    if [[ -n "${in_file}" ]]; then
        warn "  ${f} sets ${in_file} as well; the shell's value wins."
    fi
    warn "  If they belong to another cluster, stop now (Ctrl-C), open a new terminal,"
    warn "  or run: unset ${found}"
}

# _gentian_setting_source <VAR> — where a setting came from, in words.
_gentian_setting_source() {
    local v="$1" f="${INSTALL_CONFIG_FILE:-}"
    if [[ "${_GENTIAN_ENV_IDENTITY_VARS}" == *" ${v} "* ]]; then
        printf 'exported in this shell'
    elif [[ "${INSTALL_AUTO_LOAD_CONFIG:-1}" == "1" && -n "${f}" && -r "${f}" ]] &&
         grep -qE "^[[:space:]]*(export[[:space:]]+)?${v}=" "${f}"; then
        printf 'from %s' "${f}"
    else
        printf "the installer's default"
    fi
}

# _gentian_unset_hint — the line that tells a person how to drop what the
# shell brought, or nothing when it brought nothing.
_gentian_unset_hint() {
    local names="${_GENTIAN_ENV_IDENTITY_VARS# }"
    names="${names% }"
    [[ -n "${names}" ]] || return 0
    error "  - this shell exports ${names}: open a new terminal, or run"
    error "      unset ${names}"
    error "    and run the installer again."
}

# =============================================================================
# Repository addresses
# =============================================================================

# _gentian_url_redact <url> — the address with any user:token@ removed.
# Every address this file prints goes through it: an https remote may carry
# the push token in its userinfo.
_gentian_url_redact() {
    local u="$1" scheme rest auth
    case "${u}" in
        *://*)
            scheme="${u%%://*}"
            rest="${u#*://}"
            auth="${rest%%/*}"
            if [[ "${auth}" == *@* ]]; then
                rest="${auth##*@}${rest#"${auth}"}"
            fi
            printf '%s://%s' "${scheme}" "${rest}"
            ;;
        *) printf '%s' "${u}" ;;
    esac
}

# _gentian_repo_key <url> — one spelling for one repository, to compare.
#
#   https://user:tok@GitHub.com/org/repo.git  -> github.com/org/repo
#   git@github.com:org/repo                   -> github.com/org/repo
#   ssh://git@github.com:22/org/repo.git      -> github.com/org/repo
#   /srv/git/repo.git/  file:///srv/git/repo  -> /srv/git/repo
_gentian_repo_key() {
    local u="$1" auth path
    u="${u%/}"; u="${u%.git}"; u="${u%/}"
    case "${u}" in
        *://*)
            u="${u#*://}"
            auth="${u%%/*}"
            path="${u#"${auth}"}"
            ;;
        /*)
            auth=""; path="${u}"
            ;;
        *:*)
            # scp-like: [user@]host:path
            auth="${u%%:*}"
            path="/${u#*:}"
            ;;
        *)
            auth=""; path="${u}"
            ;;
    esac
    auth="${auth##*@}"
    auth="${auth%%:*}"
    auth="$(printf '%s' "${auth}" | tr '[:upper:]' '[:lower:]')"
    path="${path#/}"
    if [[ -n "${auth}" ]]; then printf '%s/%s' "${auth}" "${path}"; else printf '/%s' "${path}"; fi
}

# Read-only runs report and stop; they never ask.
_gentian_guard_read_only() {
    gentian_read_only || [[ "${GENTIAN_DIRECTION:-}" == "status" ]]
}

# =============================================================================
# A — the checkout is the repository the settings name
# =============================================================================

# gentian_check_deployments_checkout — before the checkout is synced or read.
#
# Its origin must be GENTIAN_DEPLOYMENTS_REPO and its branch
# GENTIAN_DEPLOYMENTS_BRANCH. A checkout that does not exist yet, or has no
# origin, has nothing to disagree with; the sync and the scaffold say what
# they say about those. A setting that is not known yet (asked for later)
# is not compared, and the check runs again once it is.
gentian_check_deployments_checkout() {
    resolve_deployments_path
    local path="${GENTIAN_DEPLOYMENTS_PATH}"
    local want_repo="${GENTIAN_DEPLOYMENTS_REPO:-}" want_branch="${GENTIAN_DEPLOYMENTS_BRANCH:-}"
    local key="${path}|${want_repo}|${want_branch}"
    [[ "${_GENTIAN_CHECKOUT_CHECKED}" == "${key}" ]] && return 0

    local have_repo="" have_branch="" bad=0
    # No checkout yet, or one with no origin: nothing to disagree with.
    if [[ -d "${path}/.git" ]]; then
        have_repo="$(git -C "${path}" remote get-url origin 2>/dev/null || true)"
        have_branch="$(git -C "${path}" branch --show-current 2>/dev/null || true)"
    fi

    if [[ -n "${want_repo}" && -n "${have_repo}" ]] &&
       [[ "$(_gentian_repo_key "${have_repo}")" != "$(_gentian_repo_key "${want_repo}")" ]]; then
        bad=1
        echo "" >&2
        error "The deployments checkout is a different repository from the one this run's settings name."
        error "  checkout:         ${path}   (GENTIAN_DEPLOYMENTS_PATH, $(_gentian_setting_source GENTIAN_DEPLOYMENTS_PATH))"
        error "  its origin:       $(_gentian_url_redact "${have_repo}")"
        error "  settings name:    $(_gentian_url_redact "${want_repo}")   (GENTIAN_DEPLOYMENTS_REPO, $(_gentian_setting_source GENTIAN_DEPLOYMENTS_REPO))"
        error "  settings loaded:  ${INSTALL_CONFIG_FILE:-<none>}"
    fi
    if [[ -n "${want_branch}" && -n "${have_branch}" && "${have_branch}" != "${want_branch}" ]]; then
        bad=1
        echo "" >&2
        error "The deployments checkout is on branch ${have_branch}; this run's settings name ${want_branch}."
        error "  checkout:         ${path}"
        error "  GENTIAN_DEPLOYMENTS_BRANCH, $(_gentian_setting_source GENTIAN_DEPLOYMENTS_BRANCH)"
    fi
    if (( bad )); then
        error "Stopped before the checkout was synced or read: nothing was written, committed or applied."
        error "What to do:"
        _gentian_unset_hint
        error "  - run ./install.sh from the gentian-os checkout whose install.env belongs to that repository,"
        error "    or correct GENTIAN_DEPLOYMENTS_PATH / GENTIAN_DEPLOYMENTS_REPO in ${INSTALL_CONFIG_FILE:-install.env}."
        if [[ -n "${want_branch}" && -n "${have_branch}" && "${have_branch}" != "${want_branch}" ]]; then
            error "  - if the checkout is right and only its branch is not: git -C ${path} switch ${want_branch}"
        fi
        return 1
    fi
    # Only once every setting it compares is known: a run that asks for the
    # address later comes back here with it.
    [[ -n "${want_repo}" && -n "${want_branch}" ]] && _GENTIAN_CHECKOUT_CHECKED="${key}"
    return 0
}

# _gentian_defined_clusters <path> — the cluster ids a checkout defines.
_gentian_defined_clusters() {
    local d
    for d in "$1"/clusters/*/; do
        [[ -d "${d}" ]] || continue
        d="${d%/}"
        printf '%s\n' "${d##*/}"
    done
}

# _gentian_check_definition_present — A, the second half: a repository that
# defines other clusters is not given a new one by accident.
#
# A repository with no clusters/ at all is new, and step 0 writes the first
# definition into it as it always has.
_gentian_check_definition_present() {
    local path="${GENTIAN_DEPLOYMENTS_PATH}" id="${GENTIAN_DEPLOYMENTS_CLUSTER_ID}"
    [[ -d "${path}/clusters/${id}" ]] && return 0
    local others
    others="$(_gentian_defined_clusters "${path}" | tr '\n' ' ')"
    others="${others% }"
    [[ -n "${others}" ]] || return 0

    local repo
    repo="$(_gentian_url_redact "${GENTIAN_DEPLOYMENTS_REPO:-$(git -C "${path}" remote get-url origin 2>/dev/null || echo "<no origin>")}")"

    if [[ "${GENTIAN_ADD_CLUSTER:-}" == "${id}" ]]; then
        warn "clusters/${id} is new to ${repo}, which defines: ${others}."
        warn "  GENTIAN_ADD_CLUSTER=${id} says to add it; step 0 writes it there."
        return 0
    fi

    echo ""
    warn "This repository defines other clusters, and not this one:"
    warn "  repository:   ${repo}"
    warn "  checkout:     ${path}   (GENTIAN_DEPLOYMENTS_PATH, $(_gentian_setting_source GENTIAN_DEPLOYMENTS_PATH))"
    warn "  defines:      ${others}"
    warn "  this run:     ${id}   (GENTIAN_DEPLOYMENTS_CLUSTER_ID, $(_gentian_setting_source GENTIAN_DEPLOYMENTS_CLUSTER_ID))"
    warn "  When the settings of one cluster meet another cluster's repository, this is how it looks."

    if ! _gentian_guard_read_only && [[ "${GENTIAN_NONINTERACTIVE:-0}" != "1" ]]; then
        local answer=""
        # Printed rather than given to read -p, which shows it only to a
        # terminal: the question belongs in a transcript too.
        printf "  Write a new cluster '%s' into %s at %s? [y/N] " "${id}" "${repo}" "${path}"
        read -r answer || answer=""
        case "${answer}" in
            y|Y|yes|YES|Yes) info "Adding clusters/${id} to ${repo}."; return 0 ;;
        esac
    fi

    error "Not writing clusters/${id} into ${repo}. Nothing was written, committed or applied."
    error "What to do:"
    _gentian_unset_hint
    error "  - run ./install.sh from the gentian-os checkout whose install.env names this repository's cluster."
    error "  - to add '${id}' to this repository on purpose: answer y when asked, or, unattended,"
    error "      export GENTIAN_ADD_CLUSTER=${id}"
    return 1
}

# =============================================================================
# B — the cluster kubectl reaches is the one the definition describes
# =============================================================================

# _gentian_kube_context, _gentian_kube_server — what kubectl is using, read from
# the kubeconfig only.
_gentian_kube_context() { kubectl config current-context 2>/dev/null || echo "<none>"; }
_gentian_kube_server() {
    kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo "<unknown>"
}

# _gentian_cluster_answers — the Cluster objects the cluster holds, one
# "name<TAB>kernelDomain" per line. Empty, and status 0, when there are none
# or the cluster cannot be asked: a first install, or nothing to compare.
_gentian_cluster_answers() {
    kubectl get cluster.gentianos.io -n "${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}" \
        --request-timeout=15s \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.kernelDomain}{"\n"}{end}' 2>/dev/null || true
}

# gentian_check_one_cluster [scaffold] — A's second half (with `scaffold`,
# where step 0 would write a missing definition) and B.
#
# Runs after the checkout is synced and before anything is read for the
# cluster: its credentials, its vault, its salt. Without a cluster id yet it
# does nothing and is called again once one has been asked for.
gentian_check_one_cluster() {
    local mode="${1:-}"
    local id="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}"
    [[ -n "${id}" ]] || return 0
    resolve_deployments_path
    local path="${GENTIAN_DEPLOYMENTS_PATH}" ctx key
    ctx="$(_gentian_kube_context)"
    key="${path}|${id}|${ctx}"
    [[ "${_GENTIAN_ONE_CLUSTER_KEY}" == "${key}" ]] && return 0
    _GENTIAN_CLUSTER_IDENTITY=""

    if [[ "${mode}" == "scaffold" ]]; then
        _gentian_check_definition_present || return 1
    fi

    local claim_file="${path}/clusters/${id}/kernel/claims/cluster.yaml"
    if [[ ! -r "${claim_file}" ]]; then
        # No definition yet: step 0 writes one, and there is nothing in git
        # to hold the cluster to.
        _GENTIAN_CLUSTER_IDENTITY="unasked"
        _GENTIAN_ONE_CLUSTER_KEY="${key}"
        return 0
    fi

    local want_name want_domain answers line name domain first=""
    want_name="$(yq_get '.metadata.name' "${claim_file}" 2>/dev/null || true)"
    want_domain="$(yq_get '.spec.kernelDomain' "${claim_file}" 2>/dev/null || true)"
    answers="$(_gentian_cluster_answers)"
    if [[ -z "${answers}" ]]; then
        info "Cluster identity: ${ctx} has no Cluster object yet (not installed, or not answering); nothing to compare."
        _GENTIAN_CLUSTER_IDENTITY="unasked"
        _GENTIAN_ONE_CLUSTER_KEY="${key}"
        return 0
    fi

    while IFS= read -r line; do
        [[ -n "${line}" ]] || continue
        name="${line%%$'\t'*}"
        domain=""
        [[ "${line}" == *$'\t'* ]] && domain="${line#*$'\t'}"
        [[ -n "${first}" ]] || first="${name}"$'\t'"${domain}"
        if [[ ( -z "${want_name}" || "${name}" == "${want_name}" ) &&
              ( -z "${want_domain}" || "${domain}" == "${want_domain}" ) ]]; then
            success "Cluster identity: ${ctx} is ${name} (${domain}), the cluster clusters/${id} describes."
            _GENTIAN_CLUSTER_IDENTITY="verified"
            _GENTIAN_ONE_CLUSTER_KEY="${key}"
            return 0
        fi
    done <<< "${answers}"

    name="${first%%$'\t'*}"
    domain="${first#*$'\t'}"
    if [[ -n "${GENTIAN_ACCEPT_CLUSTER:-}" && "${GENTIAN_ACCEPT_CLUSTER}" == "${name}" ]]; then
        warn "Cluster identity: ${ctx} answers as ${name} (${domain}); clusters/${id} describes ${want_name:-?} (${want_domain:-?})."
        warn "  GENTIAN_ACCEPT_CLUSTER=${name} says they are the same cluster; going on."
        _GENTIAN_CLUSTER_IDENTITY="verified"
        _GENTIAN_ONE_CLUSTER_KEY="${key}"
        return 0
    fi

    local exported_kubeconfig=""
    if [[ "${_GENTIAN_ENV_IDENTITY_VARS}" == *" KUBECONFIG "* ]]; then
        exported_kubeconfig="   (KUBECONFIG is exported in this shell)"
    fi
    echo "" >&2
    error "The cluster kubectl reaches is not the cluster this run's definition describes."
    error "  kube context:       ${ctx}${exported_kubeconfig}"
    error "  API server:         $(_gentian_kube_server)"
    error "  the definition:     ${want_name:-?}, kernel domain ${want_domain:-?}   (clusters/${id} in ${path})"
    error "  the cluster says:   ${name}, kernel domain ${domain:-?}"
    error "  settings loaded:    ${INSTALL_CONFIG_FILE:-<none>}   (gentian-os checkout ${SCRIPT_DIR:-?})"
    error "Stopped before any credential was read, cached or written, and before anything was applied."
    error "What to do:"
    _gentian_unset_hint
    error "  - to reach ${want_name:-the defined cluster}: kubectl config get-contexts, then"
    error "      kubectl config use-context <its context>"
    error "  - to work on ${name}: run ./install.sh from the gentian-os checkout whose install.env names it."
    error "  - only if ${name} really is the cluster clusters/${id} describes (its domain was changed):"
    error "      export GENTIAN_ACCEPT_CLUSTER=${name}"
    return 1
}

# gentian_cluster_identity_verified — B found the definition's Cluster object
# on the cluster kubectl reaches (or GENTIAN_ACCEPT_CLUSTER named it).
gentian_cluster_identity_verified() {
    [[ "${_GENTIAN_CLUSTER_IDENTITY}" == "verified" ]]
}

# gentian_one_cluster_confirmed — A and B have both run for the current
# settings and neither found a contradiction.
gentian_one_cluster_confirmed() {
    [[ -n "${_GENTIAN_CHECKOUT_CHECKED}" && -n "${_GENTIAN_ONE_CLUSTER_KEY}" ]] || return 1
    [[ "${_GENTIAN_CHECKOUT_CHECKED}" == "${GENTIAN_DEPLOYMENTS_PATH:-}|${GENTIAN_DEPLOYMENTS_REPO:-}|${GENTIAN_DEPLOYMENTS_BRANCH:-}" ]] || return 1
    [[ "${_GENTIAN_ONE_CLUSTER_KEY}" == "${GENTIAN_DEPLOYMENTS_PATH:-}|${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}|"* ]]
}
