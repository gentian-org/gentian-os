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
#   1. No credential value is written to local disk. There is no cache. Re-run
#      recovery comes from OpenBao (try_load_creds_from_openbao) and step state
#      comes from the cluster (check()), so a local copy has no remaining job —
#      it was only ever a workaround for not having either.
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

# catalogue_names [phase] — requirement names, optionally filtered by phase.
catalogue_names() {
    local phase="${1:-}"
    if [[ -n "${phase}" ]]; then
        _cat_yq ".requirements[] | select(.phase == \"${phase}\") | .name"
    else
        _cat_yq '.requirements[].name'
    fi
}

# catalogue_get <name> <field> — one scalar from a requirement.
catalogue_get() {
    _cat_yq ".requirements[] | select(.name == \"$1\") | .$2"
}

# catalogue_field_keys <name> — the field keys of a requirement, in order.
catalogue_field_keys() {
    _cat_yq ".requirements[] | select(.name == \"$1\") | .fields[].key"
}

# catalogue_field_attr <name> <key> <attr>
catalogue_field_attr() {
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
        master-password/password)          echo MASTER_PASSWORD ;;
        master-password/salt)              echo DERIVATION_SALT ;;
        deployments-repository/username)   echo GENTIAN_DEPLOYMENTS_GIT_USERNAME ;;
        deployments-repository/token)      echo GENTIAN_DEPLOYMENTS_GIT_TOKEN ;;
        infra-chart-registry/username)     echo REGISTRY_USER ;;
        infra-chart-registry/password)     echo REGISTRY_PASSWORD ;;
        acme-dns-cloudflare/api-token)     echo CF_API_TOKEN ;;
        smtp-relay/username)               echo SMTP_RELAY_USERNAME ;;
        smtp-relay/password)               echo SMTP_RELAY_PASSWORD ;;
        smtp-relay/host)                   echo EXTERNAL_SMTP_HOST ;;
        smtp-relay/port)                   echo EXTERNAL_SMTP_PORT ;;
        *)                                 echo "" ;;
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
        smtp)
            [[ -n "${SMTP_RELAY_PASSWORD:-}" ]] || return 0
            run_validator smtp "${EXTERNAL_SMTP_HOST:-}" "${EXTERNAL_SMTP_PORT:-587}" \
                "${SMTP_RELAY_USERNAME:-}" "${SMTP_RELAY_PASSWORD}"
            ;;
        *)
            error "Requirement '${name}' declares validator '${vtype}', which the installer does not implement."
            error "  Bootstrap validators are curl/openssl only — reclassify it as phase: runtime."
            return 1
            ;;
    esac
}

# =============================================================================
# Collection
# =============================================================================

# _prompt_field <requirement> <key> — ask for one field, unless it is already
# set from the environment, install.env, or OpenBao.
_prompt_field() {
    local req="$1" key="$2" var secret minlen example label value

    var="$(_env_var_for "${req}" "${key}")"
    [[ -n "${var}" ]] || return 0

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

    while :; do
        # Secret fields are read without echo, so a shoulder-surfer and a
        # scrollback buffer see nothing.
        if [[ "${secret}" == "true" ]]; then
            read -rsp "${prompt}" value; echo ""
        else
            read -rp "${prompt}" value
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
# Replaces prompt_credentials, prompt_kernel_secrets and the hardcoded
# MASTER_PASSWORD length check that used to live in two places.
collect_bootstrap_credentials() {
    if [[ ! -f "${GENTIAN_CATALOGUE_FILE}" ]]; then
        error "Credential catalogue not found: ${GENTIAN_CATALOGUE_FILE}"
        return 1
    fi

    banner "Credentials"
    info "From ${GENTIAN_CATALOGUE_FILE##*/} — phase: bootstrap only."
    info "Nothing is written to this machine; values go straight to OpenBao."

    local req key
    while IFS= read -r req; do
        [[ -n "${req}" ]] || continue
        while IFS= read -r key; do
            [[ -n "${key}" ]] || continue
            _prompt_field "${req}" "${key}"
        done < <(catalogue_field_keys "${req}")
    done < <(catalogue_names bootstrap)

    # Validate only after everything is collected, so an operator answering a
    # long form is not stopped halfway by a probe against a host they have not
    # been asked about yet.
    echo ""
    info "Validating supplied credentials against their endpoints..."
    local failed=0
    while IFS= read -r req; do
        [[ -n "${req}" ]] || continue
        if _validate_requirement "${req}"; then
            success "${req}"
        else
            failed=$((failed + 1))
        fi
    done < <(catalogue_names bootstrap)

    if [[ ${failed} -gt 0 ]]; then
        echo ""
        error "${failed} credential(s) failed validation. Nothing has been applied to the cluster."
        error "Fix the values above and re-run — aborting here costs nothing."
        return 1
    fi

    # DERIVATION_SALT is generated rather than supplied on a first install; on a
    # re-run try_load_creds_from_openbao has already recovered it.
    if [[ -z "${DERIVATION_SALT:-}" ]]; then
        DERIVATION_SALT="$(openssl rand -hex 16)"
        export DERIVATION_SALT
        info "Generated a new derivation salt."
    fi

    echo ""
    success "Credentials collected and validated."
}
