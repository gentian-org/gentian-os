#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-claim-defaults.sh — one default set, not two
# =============================================================================
# Seven cluster settings are declared on the Cluster XRD, which gives each a
# default, and read by the installer through claim_setting, which falls back to
# that same default via xrd_default. So there is exactly one place a default
# lives — until a shell site writes ${NETWORK_MODE:-tunnel} and creates a second
# one that agrees only until either moves.
#
# That is not hypothetical. Two failures in a single day came from a value
# resolving differently in two places:
#
#   gentian_mail_namespace answered gentian-<env> while the mail charts deployed
#   into platform-kernel, so D-03 looked for its own ConfigMap in an empty
#   namespace and called a working mail stack missing.
#
#   The portal reconstructed a tenant admin address as admin-<tenant>@gentian.org
#   because spec.adminEmail is empty whenever the address is derived — printing a
#   login for an account that exists in no realm.
#
# Both were a second derivation of something already decided elsewhere. A literal
# default in shell is the same mistake in a smaller space.
#
# Reported, not fatal: there are 63 of these today, they are individually
# harmless, and each needs a judgement about whether the site should read the
# claim, error, or legitimately keep a local default. The number must only go
# down — the same contract lint-portability ran under until it reached zero.
#
# Usage:
#   scripts/lint/lint-claim-defaults.sh [--strict]
# =============================================================================

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

STRICT=0
[[ "${1:-}" == "--strict" ]] && STRICT=1

RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; GREEN=$'\033[0;32m'; DIM=$'\033[2m'; NC=$'\033[0m'

# Fields the Cluster XRD models and claim_setting resolves. A variable belongs
# here when the claim can answer for it — that is what makes a shell default a
# second opinion rather than the only one.
CLAIM_BACKED=(
    TENANCY_MODE
    NETWORK_MODE
    ROUTING_MODE
    SECRET_MODE
    NODE_IP
    STORAGE_CLASS
    MAIL_SERVICE_MODE
)

# This file names every variable it looks for, so scanning itself would report
# one violation per rule — the mistake lint-portability made until it excluded
# itself.
files() {
    git ls-files '*.sh' 2>/dev/null |
        grep -v '^scripts/lint/lint-claim-defaults\.sh$'
}

echo ""
echo "Claim default lint — does anything but the XRD decide a default?"
echo ""

# A field's default, straight from the XRD. The variable names differ from the
# field names, so the mapping is spelled out rather than derived.
yq_default() {
    local field
    case "$1" in
        TENANCY_MODE)      field=tenancyMode ;;
        NETWORK_MODE)      field=networkMode ;;
        ROUTING_MODE)      field=routingMode ;;
        SECRET_MODE)       field=secretMode ;;
        NODE_IP)           field=nodeIp ;;
        STORAGE_CLASS)     field=storageClass ;;
        MAIL_SERVICE_MODE) field=mail.properties.serviceMode ;;
        *) return 0 ;;
    esac
    yq eval ".spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.${field}.default" \
        crossplane/xrds/cluster.yaml 2>/dev/null | grep -v '^null$' || true
}

total=0
contradictions=0
# `while read`, not mapfile: bash 3.2 has no mapfile and macOS ships 3.2.
_files=()
while IFS= read -r _f; do
    [[ -n "${_f}" ]] && _files+=("${_f}")
done < <(files)

for var in "${CLAIM_BACKED[@]}"; do
    # ${VAR:-something}. The bare ${VAR:-} form is not a default: it is "empty
    # if unset", which is what a caller writes when absence is a legal answer.
    hits="$(grep -nE "\\\$\{${var}:-[^}]" "${_files[@]}" 2>/dev/null |
            grep -vE ':[0-9]+:[[:space:]]*#' || true)"
    n=0
    [[ -n "${hits}" ]] && n="$(printf '%s\n' "${hits}" | wc -l | tr -d ' ')"
    total=$((total + n))

    # What the XRD says, so a literal can be compared with it rather than merely
    # counted. This is the half that catches a real fault: a literal that RESTATES
    # the schema is duplication, one that CONTRADICTS it is a value resolving two
    # ways depending on which code path got there first.
    want="$(yq_default "${var}")"

    if [[ ${n} -eq 0 ]]; then
        printf '  %s✓%s %-20s %s\n' "${GREEN}" "${NC}" "${var}" "0"
        continue
    fi

    # Literals that disagree with the schema, extracted from the hits above.
    disagree="$(printf '%s\n' "${hits}" |
        grep -oE "\\\$\{${var}:-[^}]*\}" |
        sed -e "s/^\\\${${var}:-//" -e 's/}$//' |
        sort -u |
        { [[ -n "${want}" ]] && grep -vx "${want}" || cat; } || true)"

    if [[ -n "${disagree}" ]]; then
        printf '  %s✗%s %-20s %-4s %sCONTRADICTS the XRD default (%s)%s\n' \
            "${RED}" "${NC}" "${var}" "${n}" "${RED}" "${want:-none}" "${NC}"
        printf '%s\n' "${disagree}" | sed 's/^/        literal: /'
        contradictions=$((contradictions + 1))
    else
        printf '  %s○%s %-20s %-4s %srestates the XRD default (%s)%s\n' \
            "${YELLOW}" "${NC}" "${var}" "${n}" "${DIM}" "${want:-none}" "${NC}"
    fi
    printf '%s\n' "${hits}" | head -2 | sed 's/^/        /'
    [[ ${n} -gt 2 ]] && printf '        %s… %d more%s\n' "${DIM}" "$((n - 2))" "${NC}"
done

echo ""
if (( total == 0 )); then
    echo "${GREEN}The XRD is the only default set.${NC}"
    exit 0
fi
if (( contradictions > 0 )); then
    echo "${RED}${contradictions} setting(s) with a literal that contradicts the XRD.${NC}"
    echo "A value that resolves one way in the Composition and another in the shell is"
    echo "the shape that made D-03 report a working mail stack missing. This one fails."
    exit 1
fi
echo "${YELLOW}${total} shell default(s) for claim-backed settings, all restating the schema.${NC}"
echo "Each is a second opinion on a value the Cluster XRD already answers."
echo "The number must only go down — see docs/plans/config-and-credential-cleanup.md §10c."
(( STRICT )) && { echo "${RED}--strict: failing.${NC}"; exit 1; }
exit 0
