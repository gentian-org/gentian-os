#!/usr/bin/env bash
# =============================================================================
# scripts/lib/credentials.sh — catalogue-driven credential collection
# =============================================================================
# Prompting iterates credentials.yaml. Adding a credential is a YAML edit; no
# shell changes, no new prompt function, no third place to forget.
#
# Everything here runs BEFORE the first step's apply(), so aborting is free: a
# credential that fails validation stops the run with the cluster untouched.
#
# Two properties this file is built around:
#
#   1. OpenBao holds the credentials, and re-run recovery comes from it
#      (try_load_creds_from_openbao); step state comes from the cluster
#      (check()). The one gap is the bootstrap itself: the KV mount does not
#      exist until B-08 creates it and B-10 seeds it, so a run that fails before
#      then has nothing to recover from. A cache covers exactly that window and
#      B-10 deletes it — see "Bootstrap credential cache" below.
#
#   2. Only `phase: bootstrap` credentials are collected. Everything else is
#      `phase: runtime` and belongs to the on-cluster credential manager, which
#      has SDKs and a real HTTP client. Keeping the shell half small is what
#      stops the two implementations drifting.
# =============================================================================

[[ -n "${GENTIAN_CREDENTIALS_LOADED:-}" ]] && return 0
GENTIAN_CREDENTIALS_LOADED=1

GENTIAN_CATALOGUE_FILE="${GENTIAN_CATALOGUE_FILE:-${SCRIPT_DIR}/credentials.yaml}"

# _cat_yq <filter> — read the catalogue, tolerant of either yq flavor.
_cat_yq() {
    local out
    if out=$(yq eval "$1" "${GENTIAN_CATALOGUE_FILE}" 2>/dev/null) && [[ "${out}" != "null" ]]; then
        echo "${out}"; return 0
    fi
    if out=$(yq -r "$1" "${GENTIAN_CATALOGUE_FILE}" 2>/dev/null) && [[ "${out}" != "null" ]]; then
        echo "${out}"; return 0
    fi
    return 1
}

# =============================================================================
# The DNS provider's credential, read from kernel/platforms.yaml
#
# credentials.yaml does not list it. Every DNS provider already declares its
# OpenBao path and fields there, next to the cert-manager solver that reads
# them, and restating that here would be one fact in two files — the drift that
# put a Secret name in an issuer and a different one in an ExternalSecret.
#
# Only the ACTIVE provider is exposed to the prompt loop. The catalogue
# describes the platform, so it carries all of them (see
# scripts/gen/gen-credential-requirements.py); an installer that asked for all
# of them would ask an operator on Route 53 for a Cloudflare token.
# =============================================================================
GENTIAN_PLATFORMS_FILE="${GENTIAN_PLATFORMS_FILE:-${SCRIPT_DIR}/kernel/platforms.yaml}"

_dns_requirement_name() {
    local provider="${DNS_PROVIDER:-cloudflare}"
    [[ "${provider}" == "none" ]] && return 1
    yq_get ".dnsProviders.${provider}.credential.vaultPath" "${GENTIAN_PLATFORMS_FILE}" \
        >/dev/null 2>&1 || return 1
    echo "acme-dns-${provider}"
}

# _dns_req <name> — the platforms.yaml path for a requirement this file owns,
# or nothing when the name belongs to credentials.yaml.
_dns_req_path() {
    local want; want="$(_dns_requirement_name || true)"
    [[ -n "${want}" && "$1" == "${want}" ]] || return 1
    echo ".dnsProviders.${want#acme-dns-}.credential"
}

# catalogue_names [phase] — requirement names, optionally filtered by phase.
catalogue_names() {
    local phase="${1:-}" dns_name dns_phase
    if [[ -n "${phase}" ]]; then
        _cat_yq ".requirements[] | select(.phase == \"${phase}\") | .name"
    else
        _cat_yq '.requirements[].name'
    fi
    dns_name="$(_dns_requirement_name || true)"
    if [[ -n "${dns_name}" ]]; then
        dns_phase="$(catalogue_get "${dns_name}" phase)"
        [[ -z "${phase}" || "${phase}" == "${dns_phase}" ]] && echo "${dns_name}"
    fi
    return 0
}

# catalogue_get <name> <field> — one scalar from a requirement.
catalogue_get() {
    local base
    if base="$(_dns_req_path "$1")"; then
        case "$2" in
            # Constant for every DNS credential: a cluster uses at most one, and
            # a cluster on HTTP-01 or a private CA uses none.
            scope)    echo cluster ;;
            optional) echo true ;;
            phase)    yq_get "${base}.phase" "${GENTIAN_PLATFORMS_FILE}" 2>/dev/null || echo runtime ;;
            validate.type)
                      yq_get "${base}.validate" "${GENTIAN_PLATFORMS_FILE}" 2>/dev/null || echo noop ;;
            *)        yq_get "${base}.$2" "${GENTIAN_PLATFORMS_FILE}" 2>/dev/null || true ;;
        esac
        return 0
    fi
    _cat_yq ".requirements[] | select(.name == \"$1\") | .$2"
}

# catalogue_field_keys <name> — the field keys of a requirement, in order.
catalogue_field_keys() {
    local base
    if base="$(_dns_req_path "$1")"; then
        yq_get "${base}.fields[].key" "${GENTIAN_PLATFORMS_FILE}" 2>/dev/null || true
        return 0
    fi
    _cat_yq ".requirements[] | select(.name == \"$1\") | .fields[].key"
}

# catalogue_field_attr <name> <key> <attr>
catalogue_field_attr() {
    local base
    if base="$(_dns_req_path "$1")"; then
        yq_get "${base}.fields[] | select(.key == \"$2\") | .$3" \
            "${GENTIAN_PLATFORMS_FILE}" 2>/dev/null || true
        return 0
    fi
    _cat_yq ".requirements[] | select(.name == \"$1\") | .fields[] | select(.key == \"$2\") | .$3"
}

# =============================================================================
# Mapping the catalogue onto the environment
#
# The installer's existing variables are the interface the rest of the shell
# still speaks. This table is the only place that mapping lives, and it shrinks
# to nothing once every consumer reads from OpenBao instead.
# =============================================================================
# _env_var_for <requirement> <field> — echo the env var, or nothing if the
# field has no shell consumer.
_env_var_for() {
    case "$1/$2" in
        master-password/value)             echo MASTER_PASSWORD ;;
        # No mapping for the salt: it is generated after collection, not
        # prompted for. A mapping here would put it in the prompt loop.
        deployments-repository/username)   echo GENTIAN_DEPLOYMENTS_GIT_USERNAME ;;
        deployments-repository/password)   echo GENTIAN_DEPLOYMENTS_GIT_TOKEN ;;
        infra-chart-registry/username)     echo REGISTRY_USER ;;
        infra-chart-registry/password)     echo REGISTRY_PASSWORD ;;
        # The DNS credential, whichever provider hosts the zone. CF_API_TOKEN is
        # kept for Cloudflare because it is exported by hand, written in runbooks
        # and cached on disk from earlier installs; the rest get one generated
        # name each rather than a table entry per provider.
        acme-dns-cloudflare/api-token)     echo CF_API_TOKEN ;;
        acme-dns-*/*)                      echo "GENTIAN_DNS_$(printf '%s' "$2" | tr 'a-z.-' 'A-Z__')" ;;
        smtp-relay/relay_username)         echo SMTP_RELAY_USERNAME ;;
        smtp-relay/relay_password)         echo SMTP_RELAY_PASSWORD ;;
        smtp-relay/host)                   echo EXTERNAL_SMTP_HOST ;;
        smtp-relay/port)                   echo EXTERNAL_SMTP_PORT ;;
        *)                                 echo "" ;;
    esac
}

# =============================================================================
# Bootstrap credential cache
#
# OpenBao is where credentials live, and try_load_creds_from_openbao recovers
# them on any later run — but only once the KV mount exists, which the Cluster
# XR creates at B-08 and B-10 seeds. Everything before that has no store, so a
# run that fails anywhere in phase A or early B asks for every credential again.
# On a bootstrap that takes several attempts, that is the same secrets retyped
# each time, which is where operators start pasting them into files that
# outlive the install.
#
# So: a cache with a fixed, short life. It is written after validation, read
# before prompting, and deleted the moment OpenBao holds the values, at which
# point it carries nothing OpenBao does not already have. It is a plain file
# with 0600 in a 0700 directory, because a cache that removed prompting while
# staying encrypted would need its key beside it.
#
# Set GENTIAN_NO_CREDENTIAL_CACHE=1 to keep credentials in the process only.
# =============================================================================
_credential_cache_file() {
    echo "${GENTIAN_CREDENTIAL_CACHE:-${HOME}/.gentian/bootstrap-credentials.env}"
}

_load_credential_cache() {
    [[ "${GENTIAN_NO_CREDENTIAL_CACHE:-0}" == "1" ]] && return 0
    local f; f="$(_credential_cache_file)"
    [[ -r "${f}" ]] || return 0

    # Same precedence as every other source: what is already set wins, so an
    # explicit export or a corrected value is never overwritten by the cache.
    local var val
    while IFS='=' read -r var val; do
        [[ "${var}" =~ ^[A-Z_][A-Z0-9_]*$ ]] || continue
        [[ -n "${!var:-}" ]] && continue
        printf -v "${var}" '%s' "$(printf '%b' "${val}")"
        export "${var?}"
    done < "${f}"
    info "Reusing credentials cached at ${f} (removed once OpenBao holds them)."
}

_save_credential_cache() {
    [[ "${GENTIAN_NO_CREDENTIAL_CACHE:-0}" == "1" ]] && return 0
    local f dir var
    f="$(_credential_cache_file)"
    dir="$(dirname "${f}")"
    mkdir -p "${dir}" || return 0
    chmod 0700 "${dir}" 2>/dev/null || true

    local tmp="${f}.tmp.$$"
    : >"${tmp}" && chmod 0600 "${tmp}" || return 0
    for var in "${_GENTIAN_CACHED_CREDENTIAL_VARS[@]}"; do
        [[ -n "${!var:-}" ]] || continue
        # printf %q would quote for the shell; this file is read by the loader
        # above, not sourced, so a literal newline is the only thing to escape.
        printf '%s=%s\n' "${var}" "${!var//$'\n'/\\n}" >>"${tmp}"
    done
    mv -f "${tmp}" "${f}" 2>/dev/null || { rm -f "${tmp}"; return 0; }
    info "Cached bootstrap credentials at ${f} (0600) so a re-run does not re-ask."
}

clear_credential_cache() {
    local f; f="$(_credential_cache_file)"
    [[ -e "${f}" ]] || return 0
    rm -f "${f}"
    success "Removed ${f} — OpenBao now holds these credentials."
}

# The bootstrap set, plus the salt: without the salt a recovered master password
# derives different values, which is worse than asking again.
_GENTIAN_CACHED_CREDENTIAL_VARS=(
    MASTER_PASSWORD
    DERIVATION_SALT
    GENTIAN_DEPLOYMENTS_GIT_USERNAME
    GENTIAN_DEPLOYMENTS_GIT_TOKEN
    REGISTRY_USER
    REGISTRY_PASSWORD
    CF_API_TOKEN
)

# _requirement_applies <req> — whether this cluster needs the credential at all.
#
# The catalogue describes the platform, so it can say a credential is optional
# but not whether this particular cluster uses the thing it unlocks. That answer
# comes from the cluster's own configuration, and it belongs here rather than in
# `optional:`, which would then mean two different things at once.
#
# A requirement that does not apply is never prompted for and never validated.
_requirement_applies() {
    case "$1" in
        infra-chart-registry)
            # Infra charts come from a public URL unless the cluster says
            # otherwise, and a public registry has no credential to give.
            [[ "${INFRA_CHART_PRIVATE:-false}" == "true" ]]
            ;;
        acme-dns-*)
            # Only the provider this cluster named, and only when it is going to
            # solve a DNS-01 challenge at all. On HTTP-01, a private CA or a
            # self-signed anchor there is no challenge to solve, so asking for a
            # zone credential is asking for something the cluster cannot use.
            [[ "$1" == "acme-dns-${DNS_PROVIDER:-cloudflare}" ]] || return 1
            [[ "${CERT_ISSUER_MODE:-acme-dns01}" == "acme-dns01" ]]
            ;;
        *)
            return 0
            ;;
    esac
}

# =============================================================================
# Validation
# =============================================================================

# _validate_requirement <name> — run the declared probe against the collected
# values. Returns 0 when satisfied or when there is nothing to check.
_validate_requirement() {
    local name="$1" vtype vhost vurl
    vtype="$(catalogue_get "${name}" 'validate.type' 2>/dev/null || echo noop)"
    [[ -n "${vtype}" && "${vtype}" != "null" ]] || vtype=noop
    [[ "${vtype}" == "noop" ]] && return 0

    vhost="$(catalogue_get "${name}" 'validate.host' 2>/dev/null || true)"
    vurl="$(catalogue_get "${name}" 'validate.url' 2>/dev/null || true)"

    local user pass
    case "${vtype}" in
        oci-registry)
            user="${REGISTRY_USER:-}"; pass="${REGISTRY_PASSWORD:-}"
            [[ -n "${pass}" ]] || return 0
            run_validator oci-registry "${vhost:-${INFRA_CHART_REPO:-}}" "${user}" "${pass}"
            ;;
        git-https)
            [[ -n "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" ]] || return 0
            run_validator git-https "${GENTIAN_DEPLOYMENTS_REPO:-}" \
                "${GENTIAN_DEPLOYMENTS_GIT_USERNAME:-x-access-token}" \
                "${GENTIAN_DEPLOYMENTS_GIT_TOKEN}"
            ;;
        oidc-discovery)
            [[ -n "${CF_API_TOKEN:-}" ]] || return 0
            run_validator oidc-discovery "${vurl}" "${CF_API_TOKEN}"
            ;;
        cloudflare-dns)
            [[ -n "${CF_API_TOKEN:-}" ]] || return 0
            # The zone is the kernel domain: that is the one DNS-01 solves in.
            run_validator cloudflare-dns "${CF_ZONE_NAME:-${KERNEL_DOMAIN:-}}" "${CF_API_TOKEN}"
            ;;
        *)
            error "Requirement '${name}' declares validator '${vtype}', which the installer does not implement."
            error "  Bootstrap validators are curl/openssl only. A validator that needs more"
            error "  belongs to phase: runtime, where the credential manager checks it in Go —"
            error "  which is where 'smtp' went, and why nothing here implements it."
            return 1
            ;;
    esac
}

# =============================================================================
# Collection
# =============================================================================

# _prompt_field <requirement> <key> — ask for one field, unless it is already
# set from the environment, install.env, or OpenBao.
# _requirement_source <req> — which file pre-supplied this requirement's value.
#
# install.env and the secrets file are auto-loaded when present, so a value can
# reach a run without anyone typing it — commonly a file left over from another
# cluster. Naming the file turns "fix the values above" into an instruction.
_requirement_source() {
    local req="$1" key var f
    key="$(catalogue_field_keys "${req}" | head -1)"
    var="$(_env_var_for "${req}" "${key}")"
    [[ -n "${var}" ]] || { echo "the environment"; return 0; }

    # install.env, and nothing else. The secrets file is no longer read, so
    # naming it here would report a source that cannot have supplied anything —
    # which was the point of this function.
    local f="${INSTALL_CONFIG_FILE:-}"
    if [[ -n "${f}" && -r "${f}" ]] &&
       grep -qE "^[[:space:]]*(export[[:space:]]+)?${var}=" "${f}"; then
        echo "${var} in ${f}"
        return 0
    fi
    echo "${var} in the environment"
}

# _reprompt_requirement <req> — clear a requirement's fields and ask again, so a
# rejected value can be corrected in place rather than by editing a file and
# starting the run over.
_reprompt_requirement() {
    local req="$1" key var
    for key in $(catalogue_field_keys "${req}"); do
        var="$(_env_var_for "${req}" "${key}")"
        [[ -n "${var}" ]] && unset "${var}"
    done

    for key in $(catalogue_field_keys "${req}"); do
        _prompt_field "${req}" "${key}"
    done
}

_prompt_field() {
    local req="$1" key="$2" var secret minlen example label value

    var="$(_env_var_for "${req}" "${key}")"
    if [[ -z "${var}" ]]; then
        # A field the catalogue declares and this mapping does not know. Skipping
        # it silently is how a required credential goes uncollected: nothing
        # prompts, the noop validator reports OK, and the install fails much
        # later complaining that the variable is unset.
        if [[ "$(catalogue_get "${req}" optional 2>/dev/null)" != "true" ]]; then
            error "${req}/${key} has no environment-variable mapping in _env_var_for()."
            error "  It is a required credential, so this run cannot collect it."
            return 1
        fi
        return 0
    fi

    # Already supplied — from the environment, a config file, or a previous
    # partial install recovered out of OpenBao. Invariant 3.
    if [[ -n "${!var:-}" ]]; then
        return 0
    fi

    secret="$(catalogue_field_attr "${req}" "${key}" secret 2>/dev/null || echo false)"
    minlen="$(catalogue_field_attr "${req}" "${key}" minLength 2>/dev/null || echo 0)"
    example="$(catalogue_field_attr "${req}" "${key}" example 2>/dev/null || true)"
    label="$(catalogue_get "${req}" displayName)"

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        if [[ "$(catalogue_get "${req}" optional)" == "true" ]]; then
            return 0
        fi
        error "${var} is unset and GENTIAN_NONINTERACTIVE=1 — export it and re-run."
        error "  ${label}: $(catalogue_get "${req}" description | head -2 | tr '\n' ' ')"
        exit 1
    fi

    local prompt="  ${label} — ${key}"
    [[ -n "${example}" && "${example}" != "null" ]] && prompt+=" (e.g. ${example})"
    [[ "${minlen}" -gt 0 ]] 2>/dev/null && prompt+=" [min ${minlen} chars]"
    prompt+=": "

    local read_rc
    while :; do
        # Secret fields are read without echo, so a shoulder-surfer and a
        # scrollback buffer see nothing.
        # Input is echoed, including for secret fields. These values are pasted
        # far more often than typed, and a paste that lost a character or picked
        # up a wrapped line is otherwise invisible until the endpoint probe
        # rejects it — or, for a value with no probe, until something fails days
        # later. Seeing what was entered is worth more here than hiding it from
        # a terminal the operator is already installing a cluster from.
        #
        # Set GENTIAN_MASK_SECRETS=1 to read secret fields without echo, for a
        # shared screen or a recorded session.
        read_rc=0
        if [[ "${secret}" == "true" && "${GENTIAN_MASK_SECRETS:-0}" == "1" ]]; then
            read -rsp "${prompt}" value || read_rc=$?
            echo ""
        else
            read -rp "${prompt}" value || read_rc=$?
        fi

        # No more input — a closed stdin, or a run driven from a pipe. Looping
        # would spin forever on a required field, and letting the non-zero read
        # escape aborts the whole install through the ERR trap.
        if (( read_rc != 0 )); then
            if [[ -z "${value}" ]]; then
                # An optional field is simply left unset. A required one must
                # say so: its validator treats an absent value as nothing to
                # check, so staying quiet here turns a missing credential into a
                # passing probe and a failure somewhere far away.
                [[ "$(catalogue_get "${req}" optional)" == "true" ]] && return 0
                error "${label} — ${key}: no input available."
                return 1
            fi
            break
        fi

        # Optional and left blank: accept and move on.
        if [[ -z "${value}" && "$(catalogue_get "${req}" optional)" == "true" ]]; then
            return 0
        fi
        if [[ -z "${value}" ]]; then
            warn "${key} is required."
            continue
        fi
        if ! validate_whitespace "${key}" "${value}"; then
            continue
        fi
        if [[ "${minlen}" -gt 0 && ${#value} -lt ${minlen} ]] 2>/dev/null; then
            warn "${key} must be at least ${minlen} characters."
            continue
        fi
        break
    done

    printf -v "${var}" '%s' "${value}"
    export "${var?}"
}

# collect_bootstrap_credentials — the whole of install-time credential entry.
#
# One pass over credentials.yaml, so what the installer asks for is decided by
# the catalogue rather than by a prompt written per credential. Adding a
# bootstrap credential is an entry in that file and nothing here.
collect_bootstrap_credentials() {
    if [[ ! -f "${GENTIAN_CATALOGUE_FILE}" ]]; then
        error "Credential catalogue not found: ${GENTIAN_CATALOGUE_FILE}"
        return 1
    fi

    banner "Credentials"
    info "From ${GENTIAN_CATALOGUE_FILE##*/} — phase: bootstrap only."

    # Before prompting: a previous attempt's answers, if this bootstrap has not
    # reached OpenBao yet.
    _load_credential_cache

    # Iterate over command substitution rather than `while read … done < <(…)`.
    # That form redirects the loop body's stdin to the catalogue stream, so the
    # `read` inside _prompt_field consumes the next requirement name instead of
    # the operator's answer — every prompt returns empty and the install fails
    # later complaining the variable is unset. Names and field keys never
    # contain whitespace, so word splitting is the whole of the parsing.
    local req key
    for req in $(catalogue_names bootstrap); do
        if ! _requirement_applies "${req}"; then
            # Said out loud: a prompt that silently does not happen is
            # indistinguishable from one that was skipped by mistake.
            info "${req}: not required by this cluster's configuration."
            continue
        fi
        for key in $(catalogue_field_keys "${req}"); do
            _prompt_field "${req}" "${key}"
        done
    done

    # Validate only after everything is collected, so an operator answering a
    # long form is not stopped halfway by a probe against a host they have not
    # been asked about yet.
    echo ""
    info "Validating supplied credentials against their endpoints..."
    # Same reason as the prompt loop above: this one re-prompts on failure.
    local failed=0
    for req in $(catalogue_names bootstrap); do
        _requirement_applies "${req}" || continue
        if _validate_requirement "${req}"; then
            success "${req}"
            continue
        fi

        # Successes are named, so a failure that is not leaves the operator
        # matching an endpoint URL against the catalogue to find out which
        # credential to fix.
        error "  requirement: ${req} ($(catalogue_get "${req}" displayName 2>/dev/null))"
        error "  value from:  $(_requirement_source "${req}")"

        # A value that arrived from a file was never typed here, so "fix it and
        # re-run" asks the operator to correct something they may not know they
        # supplied — and the next run reads the same file and fails identically.
        # Ask for it instead, which is what the prompt would have done had the
        # file not pre-empted it.
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" != "1" ]]; then
            if [[ "$(catalogue_get "${req}" optional 2>/dev/null)" == "true" ]]; then
                info "  Enter a replacement, or leave it blank to continue without it."
            else
                info "  Enter a replacement."
            fi
            _reprompt_requirement "${req}"
            if _validate_requirement "${req}"; then
                success "${req}"
                continue
            fi
        fi
        failed=$((failed + 1))
    done

    if [[ ${failed} -gt 0 ]]; then
        echo ""
        error "${failed} credential(s) failed validation. Nothing has been applied to the cluster."
        error "Fix the values above and re-run — aborting here costs nothing."
        return 1
    fi

    # DERIVATION_SALT is generated rather than supplied on a first install.
    #
    # Generating is only ever correct when this cluster has no salt yet: every
    # kernel credential is HMAC(master+salt, …), so a second salt silently
    # renames every derived secret the cluster is already using. That is not
    # hypothetical — the recovery path used to be try_load_creds_from_openbao,
    # which needs a root token, and E-04 revokes the only one the installer
    # knows at handover. Every re-run after handover therefore found no salt,
    # minted a fresh one, and derived an administrator password that had never
    # been valid, while Keycloak still held the one derived from the original.
    #
    # gentian_cluster_derivation_salt asks the cluster instead. The Secret it
    # reads is the one create_crossplane_secrets writes the salt to, it needs no
    # OpenBao credential, and it outlives handover — so "does this cluster
    # already have a salt" is answered by the cluster, not by whether this shell
    # happens to hold a token.
    #
    # The cluster's salt wins over one already in this shell, rather than only
    # filling a gap. A stale salt is exactly as wrong as no salt and harder to
    # notice: the credential cache is written at the end of every run, so one
    # run that minted a salt persists it to ~/.gentian, and every later run
    # loads that file, finds the variable set, and never asks the cluster. The
    # cluster is the only party that knows which salt its secrets were built
    # with, and disagreeing with it cannot produce a working credential.
    local cluster_salt
    cluster_salt="$(gentian_cluster_derivation_salt || true)"
    if [[ -n "${cluster_salt}" ]]; then
        if [[ -n "${DERIVATION_SALT:-}" && "${DERIVATION_SALT}" != "${cluster_salt}" ]]; then
            warn "The derivation salt in this shell is not this cluster's; using the cluster's."
            warn "  Anything derived from the other one — including the portal password a"
            warn "  previous run may have printed — was never valid here."
        fi
        DERIVATION_SALT="${cluster_salt}"
    fi
    if [[ -z "${DERIVATION_SALT:-}" ]]; then
        DERIVATION_SALT="$(openssl rand -hex 16)"
        info "Generated a new derivation salt."
    fi
    export DERIVATION_SALT

    # After the salt, so a resumed run derives the same values it started with.
    _save_credential_cache

    echo ""
    success "Credentials collected and validated."
}
