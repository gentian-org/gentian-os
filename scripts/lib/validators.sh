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
    local name="$1" token="${2:-}" candidate url body code count matched=""

    # The kernel domain is a HOSTNAME; Cloudflare zones are registrable domains.
    # desk.gentian.org is not a zone — gentian.org is, and the DNS-01 challenge
    # record _acme-challenge.desk.gentian.org is created inside it. Asking for a
    # zone named after the host therefore returns an empty list for a token with
    # exactly the right access, which reads as "no access to this domain".
    #
    # So walk up the labels to the closest enclosing zone, which is what
    # cert-manager's solver does with the same token. Stops before the public
    # suffix: a two-label candidate is the last one worth asking about.
    candidate="${name}"
    while [[ "${candidate}" == *.*.* || "${candidate}" == *.* ]]; do
        url="https://api.cloudflare.com/client/v4/zones?name=${candidate}"
        body="$(curl -s -w $'\n%{http_code}' --max-time "${GENTIAN_VALIDATE_TIMEOUT}" \
            -H "Authorization: Bearer ${token}" "${url}" 2>/dev/null)" || body=$'\n000'
        code="${body##*$'\n'}"
        body="${body%$'\n'*}"

        case "${code}" in
            200) ;;
            401|403)
                # Terminal: a rejected token is rejected for every zone, so
                # walking further would just resend it.
                _v_fail "${url}" "token rejected (HTTP ${code})" \
                    "$(jq -r '.errors[0].message // empty' <<<"${body}" 2>/dev/null)"
                return 1 ;;
            000) _v_fail "${url}" "unreachable" \
                     "no HTTP response within ${GENTIAN_VALIDATE_TIMEOUT}s"
                 return 1 ;;
            *)   _v_fail "${url}" "unexpected response (HTTP ${code})"
                 return 1 ;;
        esac

        count="$(jq -r '.result | length' <<<"${body}" 2>/dev/null || echo 0)"
        if [[ "${count}" != "0" ]]; then
            matched="${candidate}"
            break
        fi

        # Two labels left and still nothing: the next strip is a public suffix.
        [[ "${candidate}" == *.*.* ]] || break
        candidate="${candidate#*.}"
    done

    if [[ -z "${matched}" ]]; then
        _v_fail "https://api.cloudflare.com/client/v4/zones?name=${name}" \
            "no Cloudflare zone found for ${name}" \
            "the token authenticated, but none of ${name} up to the registrable domain is a zone it can see — check the token's Zone Resources, or set CF_ZONE_NAME to the zone by hand"
        return 1
    fi

    [[ "${matched}" != "${name}" ]] &&
        info "  Cloudflare zone for ${name}: ${matched}"
    return 0
}

# validate_image_tag <repository> <tag>
#
# A tag that does not exist is not discovered until kubelet has been retrying
# for hours. ImagePullBackOff names the image but nothing connects it back to
# the values file that chose the tag, and the visible symptom is somewhere else
# entirely — an operator that never starts, so a Gateway that is never created,
# so an install that waits out a timeout and reports success over a cluster with
# no ingress. One request answers it before anything is deployed.
validate_image_tag() {
    local repo="$1" tag="$2" registry path token code
    registry="${repo%%/*}"
    path="${repo#*/}"

    # ghcr.io is where the platform publishes. A mirror has its own auth, and
    # guessing at it would fail installs that are perfectly fine.
    if [[ "${registry}" != "ghcr.io" ]]; then
        info "Image tag check skipped: ${registry} is not ghcr.io."
        return 0
    fi

    token="$(curl -s --max-time "${GENTIAN_VALIDATE_TIMEOUT}" \
        "https://ghcr.io/token?scope=repository:${path}:pull&service=ghcr.io" 2>/dev/null \
        | jq -r '.token // empty' 2>/dev/null || true)"
    if [[ -z "${token}" ]]; then
        warn "Could not get a ghcr.io pull token; skipping the image tag check."
        return 0
    fi

    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time "${GENTIAN_VALIDATE_TIMEOUT}" \
        -H "Authorization: Bearer ${token}" \
        -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json" \
        "https://ghcr.io/v2/${path}/manifests/${tag}" 2>/dev/null || echo 000)"

    case "${code}" in
        200) return 0 ;;
        404) _v_fail "${repo}:${tag}" "no such tag in the registry" \
                 "CI publishes v1.2.3 for a release, develop/main for a branch, and develop-abc1234 for a commit — there is no stage-named tag" ;;
        000) warn "ghcr.io unreachable; skipping the image tag check for ${repo}:${tag}."
             return 0 ;;
        *)   warn "HTTP ${code} checking ${repo}:${tag}; continuing without the check."
             return 0 ;;
    esac
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
        *)
            error "Unknown validator type '${type}'."
            error "  Bootstrap validators are curl/openssl only; anything else is phase: runtime."
            return 1
            ;;
    esac
}
