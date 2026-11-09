#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-mail-listeners.sh
# =============================================================================
# Which ports the mail edge's load balancer serves, rendered with helm and no
# cluster: the mail ApplicationSet as the wrapper writes it, then the edge's
# chart with the parameters Argo CD passes it (scripts/tests/mail_listeners.py).
# =============================================================================
# Every check is a string handed to eval, so what it names is expanded there.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

echo ""
echo "mail edge: the ports its load balancer serves"
echo ""

ports() { python3 scripts/tests/mail_listeners.py ports --set mailServiceMode=system "$@" 2>&1; }

out="$(ports --set mailServiceMode=system)"
check "a claim that names no listeners: 25, 587 and 993, as before the setting" '[[ "${out}" == "25 587 993" ]]' "${out}"

out="$(ports --set-string mailSubmission= --set-string mailImaps=)"
check "the installer passes empty for a claim that names neither: 25, 587 and 993" '[[ "${out}" == "25 587 993" ]]' "${out}"

out="$(ports --set-string mailSubmission=false --set-string mailImaps=false)"
check "a claim the installer writes (both false): 25 only" '[[ "${out}" == "25" ]]' "${out}"

out="$(ports --set mailSubmission=false --set mailImaps=false)"
check "a boolean false is false, not empty: 25 only" '[[ "${out}" == "25" ]]' "${out}"

out="$(ports --set-string mailSubmission=true --set-string mailImaps=false)"
check "submission approved, imaps not: 25 and 587" '[[ "${out}" == "25 587" ]]' "${out}"

out="$(ports --set-string mailSubmission=false --set-string mailImaps=true)"
check "imaps approved, submission not: 25 and 993" '[[ "${out}" == "25 993" ]]' "${out}"

out="$(ports --set-string mailSubmission=yes)"
check "a value other than true or false is refused" 'grep -q "mailSubmission must be true or false" <<<"${out}"' "${out}"

# The bootstrap chart hands the claim's value on as the installer passes it,
# --set-string; empty stays empty.
NAMESPACES="$(mktemp)"
{ echo "namespaces:"; sed 's/^/  /' kernel/namespaces.yaml; } > "${NAMESPACES}"
boot() {
    helm template boot kernel/bootstrap/chart -f "${NAMESPACES}" -f kernel/platforms.yaml \
        --set-string appsets.enabled=true --set-string operator.enabled=false \
        --set-string headlamp.oidc.enabled=false --set-string kernelDomain=k.example \
        --set-string cluster=c --set-string versions.headlamp.chart=0.0.0 \
        --set-string versions.headlamp.repo=https://example.invalid "$@" 2>&1
}
out="$(boot --set-string mailSubmission=false --set-string mailImaps=true)"
check "the bootstrap chart hands the claim's values to the wrapper" \
    'grep -q "mailSubmission: \"false\"" <<<"${out}" && grep -q "mailImaps: \"true\"" <<<"${out}"' "${out}"
out="$(boot)"
check "the bootstrap chart hands on empty when the installer passes nothing" \
    'grep -q "mailSubmission: \"\"" <<<"${out}" && grep -q "mailImaps: \"\"" <<<"${out}"' "${out}"

# A claim the installer writes for a cluster that runs its own mail states
# both client ports closed; one for a cluster that relays names none.
SANDBOX="$(mktemp -d)"
trap 'rm -rf "${NAMESPACES}" "${SANDBOX}"' EXIT
fields() {
    env -i HOME="${SANDBOX}" PATH="${PATH}" SCRIPT_DIR="$(pwd)" "$@" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; _claim_cluster_fields' 2>&1
}
out="$(fields MAIL_SERVICE_MODE=system NETWORK_MODE=static-ip)"
check "a new claim with its own mail states submission: false and imaps: false" \
    'grep -A2 "^    listeners:$" <<<"${out}" | grep -q "^      submission: false" && grep -A2 "^    listeners:$" <<<"${out}" | grep -q "^      imaps: false"' "${out}"
out="$(fields MAIL_SERVICE_MODE=external)"
check "a new claim that relays names no listeners" '! grep -q "listeners:" <<<"${out}"' "${out}"

finish
