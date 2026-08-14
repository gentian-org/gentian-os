#!/usr/bin/env bash
# =============================================================================
# scripts/lib/validators.sh — pre-write probes for bootstrap credentials
# =============================================================================
# A credential is tested against its target endpoint BEFORE it is written to
# OpenBao. That converts a whole class of latent failure — "tenant provisioning
# stalled, the path exists, the password was pasted with a trailing newline" —
# into a red field at install time.
#
# Scope is deliberately small and fixed. These validators cover the
# `phase: bootstrap` set only; every `phase: runtime` credential is validated by
# the on-cluster credential manager, which has SDKs and a real HTTP client.
#
# The rule that keeps this file from growing: **every validator here is
# expressible in curl or openssl.** A credential needing request signing (s3) or
# a provider SDK (dns-provider) is `phase: runtime` by that fact. Two
# implementations of the same validator will drift, so the shell one is kept
# tiny rather than kept in sync.
#
# Each validator:
#   - returns 0 on success, non-zero on failure
#   - prints an actionable message naming the endpoint and the failure class
#   - distinguishes UNREACHABLE from REJECTED, because they need different fixes
#   - bounds its own timeout via the tool's flag; no timeout(1) dependency
# =============================================================================

[[ -n "${GENTIAN_VALIDATORS_LOADED:-}" ]] && return 0
GENTIAN_VALIDATORS_LOADED=1

# Seconds any single probe may take. An unreachable endpoint must not hang the
# installer behind a prompt the operator has already answered.
GENTIAN_VALIDATE_TIMEOUT="${GENTIAN_VALIDATE_TIMEOUT:-15}"

# _v_fail <endpoint> <class> <detail>
_v_fail() {
    error "Validation failed: ${2}"
    error "  endpoint: ${1}"
    [[ -n "${3:-}" ]] && error "  detail:   ${3}"
    return 1
}

# validate_whitespace <label> <value>
#
# Runs before every other check. A pasted secret with a trailing newline or a
# leading space is accepted by every endpoint that ignores surrounding
# whitespace and rejected by every one that does not, which makes it one of the
# most expensive and least obvious ways to break an install.
validate_whitespace() {
    local label="$1" value="$2"
    case "${value}" in
        ' '*|*' '|"$(printf '\t')"*|*"$(printf '\t')")
            error "${label} has leading or trailing whitespace — remove it and re-enter."
            return 1
            ;;
    esac
    if [[ "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
        error "${label} contains a newline — it was probably pasted with one. Re-enter it."
        return 1
    fi
    return 0
}

# =============================================================================
# oci-registry — curl the v2 API root with the credential
# =============================================================================
validate_oci_registry() {
    local host="$1" user="$2" pass="$3" url code
    host="${host#https://}"; host="${host#http://}"; host="${host%%/*}"
    url="https://${host}/v2/"

    local auth=()
    [[ -n "${pass}" ]] && auth=(-u "${user}:${pass}")

    code=$(curl -s -o /dev/null -w '%{http_code}' \
        --max-time "${GENTIAN_VALIDATE_TIMEOUT}" \
        "${auth[@]}" "${url}" 2>/dev/null) || code="000"

    case "${code}" in
        # 200 is an authenticated OK. Some registries answer the v2 root with a
        # token challenge even for valid credentials, so 401 alone is not proof
        # of a bad password — but 403 is a definite reject.
        200|204) return 0 ;;
        401)
            _v_fail "${url}" "credentials rejected (HTTP 401)" \
                "the registry did not accept this username/password pair"
            ;;
        403) _v_fail "${url}" "authenticated but forbidden (HTTP 403)" \
                "the credential is valid but lacks pull rights" ;;
        000) _v_fail "${url}" "unreachable" \
                "no HTTP response within ${GENTIAN_VALIDATE_TIMEOUT}s — check the host and your network" ;;
        *)   _v_fail "${url}" "unexpected response (HTTP ${code})" ;;
    esac
}

# =============================================================================
# git-https — the smart-HTTP ref advertisement, the cheapest authenticated read
# =============================================================================
validate_git_https() {
    local repo="$1" user="$2" token="$3" url code auth=()
    repo="${repo%.git}"; repo="${repo%/}"
    url="${repo}.git/info/refs?service=git-upload-pack"

    # Only send credentials when there are some. Passing -u with an empty
    # password makes a public repository answer 401, which would report a
    # perfectly good public repo as a rejected credential.
    [[ -n "${token}" ]] && auth=(-u "${user}:${token}")

    code=$(curl -s -o /dev/null -w '%{http_code}' \
        --max-time "${GENTIAN_VALIDATE_TIMEOUT}" \
        "${auth[@]}" "${url}" 2>/dev/null) || code="000"

    case "${code}" in
        200) return 0 ;;
        401|403)
            _v_fail "${repo}" "credentials rejected (HTTP ${code})" \
                "the token was refused, or lacks read access to this repository"
            ;;
        404) _v_fail "${repo}" "repository not found (HTTP 404)" \
                "private repositories also answer 404 when the token cannot see them" ;;
        000) _v_fail "${repo}" "unreachable" \
                "no HTTP response within ${GENTIAN_VALIDATE_TIMEOUT}s" ;;
        *)   _v_fail "${repo}" "unexpected response (HTTP ${code})" ;;
    esac
}

# =============================================================================
# oidc-discovery — an authenticated GET returning JSON
#
# Also serves any endpoint whose validity check is "does this token authenticate
# against this URL", such as Cloudflare's token-verify route.
# =============================================================================
# validate_cloudflare_dns <zone> <token>
#
# Asks whether the token can see the zone DNS-01 will write to, rather than what
# kind of token it is. /user/tokens/verify answers the second question: it
# accepts only user-owned tokens and rejects an account-owned one — the `cfat_`
# prefix — with "Invalid API Token", even when that token has full DNS rights on
# the zone. It also passes for a valid token with no access to this domain,
# which is the failure that actually matters.
validate_cloudflare_dns() {
    local zone="$1" token="${2:-}" url body code count
    url="https://api.cloudflare.com/client/v4/zones?name=${zone}"

    body="$(curl -s -w $'\n%{http_code}' --max-time "${GENTIAN_VALIDATE_TIMEOUT}" \
        -H "Authorization: Bearer ${token}" "${url}" 2>/dev/null)" || body=$'\n000'
    code="${body##*$'\n'}"
    body="${body%$'\n'*}"

    case "${code}" in
        200) ;;
        401|403) _v_fail "${url}" "token rejected (HTTP ${code})" \
                    "$(jq -r '.errors[0].message // empty' <<<"${body}" 2>/dev/null)"
                 return 1 ;;
        000) _v_fail "${url}" "unreachable" \
                 "no HTTP response within ${GENTIAN_VALIDATE_TIMEOUT}s"
             return 1 ;;
        *)   _v_fail "${url}" "unexpected response (HTTP ${code})"
             return 1 ;;
    esac

    count="$(jq -r '.result | length' <<<"${body}" 2>/dev/null || echo 0)"
    if [[ "${count}" == "0" ]]; then
        _v_fail "${url}" "the token cannot see zone ${zone}" \
            "it authenticated, but has no access to this domain — check the token's Zone Resources"
        return 1
    fi
    return 0
}

validate_oidc_discovery() {
    local url="$1" token="${2:-}" code auth=()
    [[ -n "${token}" ]] && auth=(-H "Authorization: Bearer ${token}")

    code=$(curl -s -o /dev/null -w '%{http_code}' \
        --max-time "${GENTIAN_VALIDATE_TIMEOUT}" \
        "${auth[@]}" "${url}" 2>/dev/null) || code="000"

    case "${code}" in
        200) return 0 ;;
        401|403) _v_fail "${url}" "token rejected (HTTP ${code})" ;;
        000) _v_fail "${url}" "unreachable" \
                "no HTTP response within ${GENTIAN_VALIDATE_TIMEOUT}s" ;;
        *)   _v_fail "${url}" "unexpected response (HTTP ${code})" ;;
    esac
}

# =============================================================================
# smtp — STARTTLS then AUTH LOGIN, via openssl s_client
# =============================================================================
validate_smtp() {
    local host="$1" port="$2" user="$3" pass="$4" out b64_user b64_pass endpoint
    endpoint="${host}:${port}"

    b64_user=$(printf '%s' "${user}" | openssl base64 -A)
    b64_pass=$(printf '%s' "${pass}" | openssl base64 -A)

    # -crlf because SMTP requires CRLF line endings; without it the server sees
    # one malformed command and the failure looks like a rejected password.
    out=$(printf 'EHLO gentian-installer\r\nAUTH LOGIN\r\n%s\r\n%s\r\nQUIT\r\n' \
            "${b64_user}" "${b64_pass}" |
          openssl s_client -quiet -crlf -starttls smtp \
            -connect "${endpoint}" 2>&1) || true

    if [[ -z "${out}" ]]; then
        _v_fail "${endpoint}" "unreachable" "no response from the relay"
        return 1
    fi
    # 235 is "authentication succeeded".
    if echo "${out}" | grep -q '235'; then
        return 0
    fi
    if echo "${out}" | grep -qE '^(535|534|530)'; then
        _v_fail "${endpoint}" "credentials rejected" \
            "$(echo "${out}" | grep -m1 -E '^(535|534|530)')"
        return 1
    fi
    _v_fail "${endpoint}" "authentication did not complete" \
        "no 235 in the relay's response; it may require a different auth mechanism"
}

# =============================================================================
# Dispatch
# =============================================================================

# run_validator <type> [args...] — dispatch by spec.validate.type.
#
# An unknown type is an error rather than a pass: silently accepting a
# credential the catalogue asked to have checked is the failure this file
# exists to prevent.
run_validator() {
    local type="$1"; shift
    case "${type}" in
        noop)           return 0 ;;
        oci-registry)   validate_oci_registry "$@" ;;
        git-https)      validate_git_https "$@" ;;
        oidc-discovery) validate_oidc_discovery "$@" ;;
        cloudflare-dns) validate_cloudflare_dns "$@" ;;
        smtp)           validate_smtp "$@" ;;
        *)
            error "Unknown validator type '${type}'."
            error "  Bootstrap validators are curl/openssl only; anything else is phase: runtime."
            return 1
            ;;
    esac
}
