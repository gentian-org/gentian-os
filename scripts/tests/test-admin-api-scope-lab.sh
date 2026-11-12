#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-admin-api-scope-lab.sh
# =============================================================================
# The master realm's service accounts keep their access to the Admin REST API
# across the upgrade to Keycloak 26.8.2.
#
# From 26.8.2 on the Admin API refuses a token that lacks the kc-admin-api
# scope and the realm's Admin API address as its audience. Keycloak makes that
# scope when it migrates and gives it only to its own consoles; the operator,
# the realm Jobs and Crossplane sign in with accounts of their own, which the
# installer makes (_keycloak_admin_clients_shell). So this runs that shell, as
# the bootstrap Job does, on a Keycloak before 26.8.2, upgrades the same
# database to 26.8.2 or the version the chart pins if later, and has the
# accounts call the Admin API -- beside an account without the scope, which
# has to be refused for the test to mean anything. Then the same on a fresh
# database, where the scope is Keycloak's own.
#
# Keycloak starts the way the chart starts it: a fixed frontend address that
# the test never calls, with every call on the backend address, as the
# cluster's Jobs and the operator call it.
#
# Needs docker, curl, jq and openssl. Not part of CI: it pulls and boots
# Keycloak three times.
# =============================================================================
# The shell under test is taken out of its library and evaluated, so what it
# reads is set here without a visible use.
# shellcheck disable=SC2016,SC2034,SC2317,SC2329
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

for tool in docker curl jq openssl; do
    command -v "${tool}" >/dev/null 2>&1 || { echo "SKIP: ${tool} is not installed"; exit 0; }
done

# The last Keycloak without the scope, and the one with it: 26.8.2, or the
# version the chart pins (keycloakx.image.tag) once that is later.
FROM="26.8.0"
PINNED="$(awk '/^keycloakx:/{kc=1; next} kc && /^[^ #]/{kc=0} kc && /^  image:/{img=1; next} img && /^  [^ ]/{img=0} img && /^    tag:/{gsub(/"/, "", $2); print $2; exit}' kernel/services/keycloak-idp/manifests/values.yaml)"
[[ -n "${PINNED}" ]] || { echo "No keycloakx.image.tag in kernel/services/keycloak-idp/manifests/values.yaml" >&2; exit 1; }
TO="$(printf '%s\n' 26.8.2 "${PINNED}" | sort -V | tail -n 1)"
PORT="${ADMIN_API_LAB_PORT:-28081}"
KC="http://127.0.0.1:${PORT}/auth"
FRONTEND="https://id.example.org/auth"
ADMIN_PW="$(openssl rand -hex 12)"
OPERATOR_SECRET="$(openssl rand -hex 16)"; PROVISIONER_SECRET="$(openssl rand -hex 16)"; BARE_SECRET="$(openssl rand -hex 16)"

cleanup() {
    docker rm -f aas-keycloak >/dev/null 2>&1
    docker volume rm aas-upgraded aas-fresh >/dev/null 2>&1
}
trap cleanup EXIT
cleanup 2>/dev/null

# keycloak <version> <volume> — starts it with the chart's arguments, on
# Keycloak's own file database in the volume, and waits until it answers.
keycloak() {
    # Stopped, not killed: the file database keeps what it has not flushed
    # only in memory, and a kill loses the previous version's data.
    docker stop -t 60 aas-keycloak >/dev/null 2>&1
    docker rm -f aas-keycloak >/dev/null 2>&1
    docker run -d --name aas-keycloak -p "127.0.0.1:${PORT}:8080" -v "$2:/opt/keycloak/data" \
        -e KC_DB=dev-file -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e "KC_BOOTSTRAP_ADMIN_PASSWORD=${ADMIN_PW}" \
        -e KC_BOOTSTRAP_ADMIN_CLIENT_ID=gentian-operator -e "KC_BOOTSTRAP_ADMIN_CLIENT_SECRET=${OPERATOR_SECRET}" \
        --entrypoint /opt/keycloak/bin/kc.sh "quay.io/keycloak/keycloak:$1" \
        start --http-enabled=true --http-relative-path=/auth "--hostname=${FRONTEND}" \
        --hostname-backchannel-dynamic=true --hostname-strict=false \
        --proxy-headers=xforwarded --cache=local >/dev/null
    for _ in $(seq 1 120); do
        curl -sf "${KC}/realms/master" >/dev/null 2>&1 && return 0
        sleep 2
    done
    docker logs --tail 30 aas-keycloak
    return 1
}

admin_token() {
    curl -sf -X POST "${KC}/realms/master/protocol/openid-connect/token" -d client_id=admin-cli \
        -d username=admin --data-urlencode "password=${ADMIN_PW}" -d grant_type=password | jq -r .access_token
}
client_token() { # <client> <secret>
    curl -sf -X POST "${KC}/realms/master/protocol/openid-connect/token" -d "client_id=$1" \
        --data-urlencode "client_secret=$2" -d grant_type=client_credentials | jq -r '.access_token // empty'
}
# admin_api_status <token> — what the Admin API answers that token.
admin_api_status() {
    curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" "${KC}/admin/realms/master/clients?max=1"
}
claims() { # the token's payload: base64url without its padding
    local p
    p="$(cut -d. -f2 <<<"$1" | tr '_-' '/+')"
    while (( ${#p} % 4 )); do p+="="; done
    base64 -d <<<"${p}" 2>/dev/null
}
scopes_named() { # how many client scopes of the master realm are called kc-admin-api
    curl -sf -H "Authorization: Bearer $(admin_token)" "${KC}/admin/realms/master/client-scopes" |
        jq '[.[] | select(.name == "kc-admin-api")] | length'
}

# installer — the bootstrap Job's part that makes the accounts: the same
# shell, with the variables the Job sets before it.
installer() (
    KEYCLOAK_BASE="${KC}"
    AUTH="Authorization: Bearer $(admin_token)"
    MASTER="${KEYCLOAK_BASE}/admin/realms/master"
    ADMIN_ROLE="$(curl -sf -H "${AUTH}" "${MASTER}/roles/admin")"
    eval "$(sed -n '/^_keycloak_admin_clients_shell() {/,/^}/p' scripts/lib/portal-login-bootstrap.sh)"
    eval "$(_keycloak_admin_clients_shell)"
    set -e
    ensure_admin_api_scope
    ensure_admin_client gentian-operator "${OPERATOR_SECRET}"
    ensure_admin_client keycloak-provisioner "${PROVISIONER_SECRET}"
)

# The accounts as the installer leaves them, beside one that is the same but
# for the scope.
bare_account() (
    KEYCLOAK_BASE="${KC}"
    AUTH="Authorization: Bearer $(admin_token)"
    MASTER="${KEYCLOAK_BASE}/admin/realms/master"
    ADMIN_ROLE="$(curl -sf -H "${AUTH}" "${MASTER}/roles/admin")"
    eval "$(sed -n '/^_keycloak_admin_clients_shell() {/,/^}/p' scripts/lib/portal-login-bootstrap.sh)"
    eval "$(_keycloak_admin_clients_shell)"
    set -e
    ensure_admin_api_scope
    ensure_admin_client lab-without-scope "${BARE_SECRET}"
    id="$(curl -sf -H "${AUTH}" "${MASTER}/clients?clientId=lab-without-scope" | jq -r '.[0].id')"
    curl -sf -X DELETE -H "${AUTH}" "${MASTER}/clients/${id}/default-client-scopes/${ADMIN_API_SCOPE_ID}" >/dev/null
)

expect_status() { # <what> <client> <secret> <status>
    local token got want="$4"
    token="$(client_token "$2" "$3")"
    got="$( [[ -n "${token}" ]] && admin_api_status "${token}" || echo "no token")"
    check "$1" '[[ "${got}" == "${want}" ]]' "  the Admin API answered ${got}"
}

echo ""
echo "Keycloak ${FROM}: the installer makes the accounts and the scope"
echo ""
docker volume create aas-upgraded >/dev/null
keycloak "${FROM}" aas-upgraded || { bad "Keycloak ${FROM} did not start"; finish; }
out="$(installer 2>&1)"; rc=$?
check "the installer's shell runs" '[[ ${rc} == 0 ]]' "${out}"
check "it makes the scope, which ${FROM} does not have" 'grep -q "Created the kc-admin-api client scope (audience ${FRONTEND}/admin/realms/master)" <<<"${out}"' "${out}"
bare_account >/dev/null 2>&1 || bad "could not make the account without the scope"
token="$(client_token gentian-operator "${OPERATOR_SECRET}")"
check "the operator's token carries the scope" 'claims "${token}" | jq -e ".scope | split(\" \") | index(\"kc-admin-api\")" >/dev/null' "$(claims "${token}")"
check "and the Admin API address as its audience" 'claims "${token}" | jq -e --arg a "${FRONTEND}/admin/realms/master" "[.aud] | flatten | index(\$a)" >/dev/null' "$(claims "${token}")"
expect_status "the operator calls the Admin API" gentian-operator "${OPERATOR_SECRET}" 200
expect_status "an account without the scope still may, before ${TO}" lab-without-scope "${BARE_SECRET}" 200

echo ""
echo "Keycloak ${TO} on the same database"
echo ""
keycloak "${TO}" aas-upgraded || { bad "Keycloak ${TO} did not start on ${FROM}'s database"; finish; }
expect_status "the operator calls the Admin API" gentian-operator "${OPERATOR_SECRET}" 200
expect_status "the provisioner calls the Admin API" keycloak-provisioner "${PROVISIONER_SECRET}" 200
expect_status "an account without the scope is refused" lab-without-scope "${BARE_SECRET}" 401
check "the admin console's client still signs in" '[[ "$(admin_api_status "$(admin_token)")" == 200 ]]'
check "the migration kept the one scope of that name" '[[ "$(scopes_named)" == 1 ]]' "  $(scopes_named) scopes"
out="$(installer 2>&1)"; rc=$?
check "the installer run again changes nothing" '[[ ${rc} == 0 ]] && ! grep -q "Created" <<<"${out}"' "${out}"
expect_status "and the operator still calls the Admin API" gentian-operator "${OPERATOR_SECRET}" 200

echo ""
echo "Keycloak ${TO} on a new database: the scope is Keycloak's own"
echo ""
docker volume create aas-fresh >/dev/null
keycloak "${TO}" aas-fresh || { bad "Keycloak ${TO} did not start"; finish; }
out="$(installer 2>&1)"; rc=$?
check "the installer's shell runs" '[[ ${rc} == 0 ]]' "${out}"
check "it takes Keycloak's scope and makes none" '! grep -q "Created the kc-admin-api" <<<"${out}" && [[ "$(scopes_named)" == 1 ]]' "${out}"
expect_status "the operator calls the Admin API" gentian-operator "${OPERATOR_SECRET}" 200
expect_status "the provisioner calls the Admin API" keycloak-provisioner "${PROVISIONER_SECRET}" 200

finish
