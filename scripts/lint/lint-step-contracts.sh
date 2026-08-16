#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-step-contracts.sh — does check() agree with provides:?
# =============================================================================
# Every install failure on the first real cluster run came from one shape: a
# check() that disagreed with its own `provides:` header. Thirteen of them.
#
#   A-09  tested the ArgoCD Deployment; the ApplicationSet CRD was missing
#   B-02  tested a Secret B-01 pre-creates as a placeholder
#   B-05  asked OpenBao a question this shell cannot ask
#   C-02  tested that an Application existed, not that it was right
#   C-06  tested the requirements; the probes were gone
#   A-02  tested one XRD of six
#   D-03  tested another step's artefact entirely
#   D-05  tested the Application; the store id had never arrived
#   A-03  demanded a namespace apply() never created
#
# Six of them meant a partial install read as complete and the step that would
# have repaired it was skipped. Each was found by a cluster failing hours later,
# somewhere else. Most were visible in the step file alone.
#
# The rules below are what can be judged without a cluster. They are deliberately
# split: ERRORs are objective and fail the build; WARNs are heuristic — a
# `provides:` noun the check never mentions is usually a gap and occasionally a
# synonym, so they are reported and counted but do not fail unless --strict.
#
# Usage:
#   scripts/lint/lint-step-contracts.sh [--strict]
# =============================================================================

set -uo pipefail
# scripts/lint/ -> repo root
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

STRICT=0
[[ "${1:-}" == "--strict" ]] && STRICT=1

RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; GREEN=$'\033[0;32m'; NC=$'\033[0m'
errors=0
warnings=0

_err()  { echo "  ${RED}ERROR${NC}  $*"; errors=$((errors + 1)); }
_warn() { echo "  ${YELLOW}WARN${NC}   $*"; warnings=$((warnings + 1)); }

# _body <file> <verb> — the text of one verb, or empty when it is not defined.
_body() {
    awk -v v="$2" '$0 ~ "^"v"\\(\\) \\{" {f=1} f {print} f && /^}/ {exit}' "$1"
}

# _header <file> <field>
_header() {
    sed -n "s/^# $2:[[:space:]]*//p" "$1" | head -1
}

# _strip_comments — a noun mentioned only in a comment is not a check.
_strip_comments() { sed 's/#.*//'; }

echo ""
echo "Step contract lint — does each check() test what its step provides?"
echo ""

shopt -s nullglob
for file in scripts/steps/*.sh; do
    id="$(basename "${file}" .sh)"
    provides="$(_header "${file}" provides)"
    check="$(_body "${file}" check | _strip_comments)"
    apply="$(_body "${file}" apply | _strip_comments)"
    destroy="$(_body "${file}" destroy | _strip_comments)"

    # ── Objective rules ──────────────────────────────────────────────────────

    [[ -n "${provides}" ]] || _err "${id}: no 'provides:' header — nothing to check against."
    [[ -n "${apply}" ]]    || _err "${id}: no apply() — a step that does nothing is not a step."

    # An unconditional `return 1` means "always run", which now has its own
    # verdict. Left as 1 it renders as missing, so a healthy cluster reports a
    # fault and the operator learns to ignore red.
    if [[ -n "${check}" ]] && grep -qE '^\s*return 1\s*$' <<<"${check}"; then
        if ! grep -q 'CHECK_ALWAYS' <<<"${check}"; then
            _err "${id}: check() returns 1 unconditionally — use CHECK_ALWAYS so it reads as 'always', not as a fault."
        fi
    fi

    # A check() that cannot run where the driver runs it. bao needs VAULT_ADDR
    # and a token, neither of which the driver sets before check() — B-05 asked
    # anyway and could never resolve, so its verdict was whatever the failure
    # happened to mean that day.
    # Command position only. `-n argocd` is a namespace and `provider-helm` is a
    # resource name; neither runs anything.
    if [[ -n "${check}" ]] && grep -qE '(^|[|;&(]|&&|\|\|)[[:space:]]*(bao|argocd|helm)[[:space:]]' <<<"${check}"; then
        _err "${id}: check() calls a tool the driver does not configure (bao/argocd/helm). Ask Kubernetes instead."
    fi

    # ── Heuristic rules ──────────────────────────────────────────────────────

    # Nouns in `provides:` that look like Kubernetes kinds or object names, and
    # whether the check mentions them at all. Catches "tested one XRD of six"
    # and "tested the requirements, not the probes".
    if [[ -n "${check}" && -n "${provides}" ]]; then
        while read -r noun; do
            [[ -n "${noun}" ]] || continue
            grep -qi -- "${noun}" <<<"${check}" ||
                _warn "${id}: provides '${noun}' but check() never mentions it."
        done < <(
            # Only the nouns this step produces. A `provides:` often names where
            # something came from — "AppProfile CRs from the gentian-apps
            # repository" — and that repository is prose, not an artefact.
            tr ' ,—' '\n' <<<"${provides%% from *}" \
            | grep -oiE '^(ApplicationSet|Application|Deployment|StatefulSet|Secret|ConfigMap|Namespace|CRD|XRD|CRDs|XRDs|Composition|Compositions|ClusterIssuers?|Gateway|Certificate|CredentialRequirement|AppProfile|AppCatalogue|Repository|Claim)s?$' \
            | tr '[:upper:]' '[:lower:]' | sed 's/s$//' | sort -u
        )
    fi

    # A check() testing an object another step's destroy() removes is testing
    # something it does not own. D-03 checked keycloak-smtp-credentials, which
    # D-05 creates — so apply() could never satisfy check().
    if [[ -n "${check}" ]]; then
        while read -r obj; do
            [[ -n "${obj}" ]] || continue
            for other in scripts/steps/*.sh; do
                [[ "${other}" == "${file}" ]] && continue
                other_destroy="$(_body "${other}" destroy | _strip_comments)"
                if grep -q -- "${obj}" <<<"${other_destroy}" &&
                   ! grep -q -- "${obj}" <<<"${apply}${destroy}"; then
                    _warn "${id}: check() tests '${obj}', which $(basename "${other}" .sh) destroys and this step never touches."
                fi
            done
        done < <(grep -oE '\b[a-z0-9]+(-[a-z0-9]+){2,}\b' <<<"${check}" | sort -u | head -6)
    fi
done

echo ""
if (( errors > 0 )); then
    echo "${RED}${errors} error(s), ${warnings} warning(s).${NC}"
    exit 1
fi
if (( warnings > 0 )); then
    echo "${YELLOW}0 errors, ${warnings} warning(s).${NC}"
    (( STRICT )) && exit 1
    exit 0
fi
echo "${GREEN}Every check() references what its step provides.${NC}"
