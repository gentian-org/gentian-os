#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-derived-key-check.sh
# =============================================================================
# A release that adds a key to a vault path B-07 derives ships the
# ExternalSecret that reads it, and Argo CD applies that on its own. B-07's
# check used to ask only whether the master-password Secret existed, so on an
# installed cluster the step was always skipped, the key never reached the
# vault, and the ExternalSecret failed until someone forced the step.
#
# Exercised with kubectl stubbed and no cluster:
#   - derived_kv_paths lists exactly the paths create_crossplane_secrets adds
#     missing keys to;
#   - external_secrets_missing_derived_keys names the ExternalSecrets on those
#     paths that are not Ready, and no others;
#   - B-07's check is missing for either reason and satisfied otherwise.
# =============================================================================

# shellcheck disable=SC2016 # the checks are strings the harness evaluates
# shellcheck disable=SC2034,SC2317,SC2329 # read and called by the code under test
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

CHECK_SATISFIED=0 CHECK_MISSING=1 CHECK_UNDEFINED=2
warn() { :; }
ns_kernel() { echo "kernel-$1"; }

# shellcheck source=scripts/lib/bootstrap.sh
source scripts/lib/bootstrap.sh

echo ""
echo "derived_kv_paths"
echo ""

listed="$(derived_kv_paths)"
# Every _kv_secret call that names a path is one _kv_add_missing writes to.
want="$(sed -n '/^create_crossplane_secrets() {/,/^}/p' scripts/lib/bootstrap.sh |
    grep -cE '^[[:space:]]+"gentian-os/[^"]+"[[:space:]]*$')"
check "as many paths as create_crossplane_secrets names" '[[ $(wc -l <<<"${listed}") == "${want}" && ${want} -gt 0 ]]' "${listed}"
check "the identity bootstrap path is one of them" 'grep -qx "gentian-os/kernel/identity/keycloak-bootstrap" <<<"${listed}"'
check "every line is a vault path" '! grep -qv "^gentian-os/" <<<"${listed}"'

echo ""
echo "external_secrets_missing_derived_keys"
echo ""

# The cluster's ExternalSecrets, as kubectl answers: one per case below.
cat >"${WORK}/es.json" <<'JSON'
{"items": [
 {"metadata": {"namespace": "kernel-authentication", "name": "keycloak-idp-credentials"},
  "spec": {"data": [{"remoteRef": {"key": "gentian-os/kernel/identity/keycloak-bootstrap", "property": "provisioner_client_secret"}}]},
  "status": {"conditions": [{"type": "Ready", "status": "False", "message": "could not get secret data from provider"}]}},
 {"metadata": {"namespace": "kernel-data", "name": "kernel-postgres-role-keycloak"},
  "spec": {"data": [{"remoteRef": {"key": "gentian-os/kernel/database/postgresql", "property": "keycloak_user_password"}}]},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}},
 {"metadata": {"namespace": "kernel-authorization", "name": "openfga-whole"},
  "spec": {"dataFrom": [{"extract": {"key": "gentian-os/kernel/authz/openfga"}}]},
  "status": {}},
 {"metadata": {"namespace": "system-authorization", "name": "openfga-apps-key"},
  "spec": {"data": [{"remoteRef": {"key": "gentian-os/kernel/authz/openfga-apps", "property": "key"}}]},
  "status": {"conditions": [{"type": "Ready", "status": "False"}]}},
 {"metadata": {"namespace": "kernel-control", "name": "gentian-director-signing"},
  "spec": {"data": [{"remoteRef": {"key": "gentian-os/kernel/signing/director", "property": "private"}}]},
  "status": {"conditions": [{"type": "Ready", "status": "False"}]}}
]}
JSON

STUB_ES="${WORK}/es.json"
MASTER_SECRET=present
kubectl() {
    case "$*" in
        "get externalsecrets.external-secrets.io -A -o json") cat "${STUB_ES}" ;;
        "get secret gentian-os-master-password"*) [[ "${MASTER_SECRET}" == present ]] ;;
        *) return 1 ;;
    esac
}

unready="$(external_secrets_missing_derived_keys)"
check "a failing reader of a derived path is named" 'grep -qx "kernel-authentication/keycloak-idp-credentials" <<<"${unready}"'
check "a reader that has never synced is named" 'grep -qx "kernel-authorization/openfga-whole" <<<"${unready}"'
check "a Ready reader is not" '! grep -q "kernel-postgres-role-keycloak" <<<"${unready}"'
check "a path that only shares a prefix is not" '! grep -q "openfga-apps-key" <<<"${unready}"'
check "a path this step does not derive is not" '! grep -q "gentian-director-signing" <<<"${unready}"'

echo ""
echo "B-07's check"
echo ""

STEP="$(echo scripts/steps/[A-Z]-[0-9][0-9]-crossplane-secrets.sh)"
[[ -f "${STEP}" ]] || { echo "FAIL — no crossplane-secrets step found." >&2; exit 1; }
# The step defines check() too, which would replace the harness's: kept
# aside as step_check, and the harness's put back.
eval "$(declare -f check | sed '1s/^check/harness_check/')"
# shellcheck source=/dev/null
source "${STEP}"
eval "$(declare -f check | sed '1s/^check/step_check/')"
eval "$(declare -f harness_check | sed '1s/^harness_check/check/')"

verdict() { rc=0; step_check || rc=$?; }

MASTER_SECRET=absent STUB_ES="${WORK}/es.json"; verdict
check "no master-password Secret: missing" '[[ ${rc} == ${CHECK_MISSING} ]]'

MASTER_SECRET=present STUB_ES="${WORK}/es.json"; verdict
check "a derived path's reader not Ready: missing" '[[ ${rc} == ${CHECK_MISSING} ]]'

jq '.items |= map(select(.metadata.name == "kernel-postgres-role-keycloak" or .metadata.name == "gentian-director-signing"))' \
    "${WORK}/es.json" >"${WORK}/ready.json"
MASTER_SECRET=present STUB_ES="${WORK}/ready.json"; verdict
check "every derived path's reader Ready: satisfied" '[[ ${rc} == ${CHECK_SATISFIED} ]]'

finish
