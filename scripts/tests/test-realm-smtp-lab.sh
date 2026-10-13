#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-realm-smtp-lab.sh
# =============================================================================
# A realm that is asked to send mail sends it, logged in to the mail server.
#
# Keycloak stores a realm's mail server whatever it is given and answers 204:
# authentication on, a login and no password is accepted, and so is a masked
# password beside a changed login. Either is found out only when somebody is
# invited, as "failed to connect, no password specified?". So this starts the
# Keycloak the chart pins beside a mail server that refuses anyone who has not
# logged in over STARTTLS, writes the realm's mail server the way the platform
# does -- the installer's request for the kernel realm, the operator's Job
# script for a tenant realm, each with the cluster's own mail and with a relay
# -- and has Keycloak send. What counts is the message arriving and the login
# the mail server saw, not what the realm reads back.
#
# Needs docker, go, curl and jq. Not part of CI: it pulls and boots Keycloak.
# =============================================================================
# The installer's two functions are taken out of their library and evaluated,
# so what they read and call is set and defined here without a visible use.
# shellcheck disable=SC2030,SC2031,SC2034,SC2329
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; fail=$((fail + 1)); }

for tool in docker go curl jq openssl; do
    command -v "${tool}" >/dev/null 2>&1 || { echo "SKIP: ${tool} is not installed"; exit 0; }
done

# The Job script is built by the operator's package, whose tests start a local
# API server: say where its binaries are when the machine has them elsewhere.
if [[ -z "${KUBEBUILDER_ASSETS:-}" && ! -d /tmp/envtest-bins/k8s/1.32.0-linux-amd64 ]] && command -v setup-envtest >/dev/null 2>&1; then
    KUBEBUILDER_ASSETS="$(setup-envtest use 1.32.0 -p path 2>/dev/null || true)"
    export KUBEBUILDER_ASSETS
fi

KC_VERSION="$(sed -n 's/^keycloakVersion: "\(.*\)"$/\1/p' kernel/services/keycloak-idp/manifests/values.yaml)"
KC_PORT="${REALM_SMTP_LAB_KC_PORT:-28080}"
MAIL_PORT="${REALM_SMTP_LAB_MAIL_PORT:-28025}"
KC="http://127.0.0.1:${KC_PORT}/auth"
MAIL="http://127.0.0.1:${MAIL_PORT}"
# Made up for this run and gone with it.
ADMIN_PW="$(openssl rand -hex 12)"
TENANT_PW="$(openssl rand -hex 16)"; KERNEL_PW="$(openssl rand -hex 16)"; RELAY_PW="$(openssl rand -hex 16)"

W="$(mktemp -d "${TMPDIR:-/tmp}/rsl.XXXXXX")"
cleanup() {
    docker rm -f rsl-keycloak rsl-mail >/dev/null 2>&1
    docker network rm rsl >/dev/null 2>&1
    rm -rf "${W}"
}
trap cleanup EXIT
cleanup 2>/dev/null; mkdir -p "${W}"

# A certificate for the name Keycloak dials, as the cluster's wildcard is, and
# trusted by Keycloak: Java checks it against that name before it will log in.
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=*.example.org" \
    -addext "subjectAltName=DNS:*.example.org" \
    -keyout "${W}/tls.key" -out "${W}/tls.crt" >/dev/null 2>&1
printf '%s\n' "noreply@team.example.org:${TENANT_PW}" "gentian-system@example.org:${KERNEL_PW}" \
    "relay-user:${RELAY_PW}" > "${W}/logins"
chmod -R a+rX "${W}"

docker network create rsl >/dev/null
docker run -d --name rsl-mail --network rsl \
    --network-alias mail.example.org --network-alias relay.example.org \
    -v "${W}:/lab:ro" -p "127.0.0.1:${MAIL_PORT}:8025" axllent/mailpit:v1.31.4 \
    --smtp 0.0.0.0:587 --smtp-auth-file /lab/logins --smtp-require-starttls \
    --smtp-tls-cert /lab/tls.crt --smtp-tls-key /lab/tls.key >/dev/null
# The chart's start arguments (keycloak-idp/manifests/templates/configmap.yaml),
# on Keycloak's own file database in place of the cluster's Postgres.
docker run -d --name rsl-keycloak --network rsl -p "127.0.0.1:${KC_PORT}:8080" \
    -v "${W}:/lab:ro" -e KC_TRUSTSTORE_PATHS=/lab/tls.crt -e KC_DB=dev-file \
    -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e "KC_BOOTSTRAP_ADMIN_PASSWORD=${ADMIN_PW}" \
    --entrypoint /opt/keycloak/bin/kc.sh "quay.io/keycloak/keycloak:${KC_VERSION}" \
    start --http-enabled=true --http-relative-path=/auth --hostname-strict=false \
    --proxy-headers=xforwarded --cache=local >/dev/null

echo ""
echo "realm mail: Keycloak ${KC_VERSION} sends through a server that demands a login"
echo ""
for _ in $(seq 1 90); do
    curl -sf "${KC}/realms/master" >/dev/null 2>&1 && break
    sleep 2
done
curl -sf "${KC}/realms/master" >/dev/null 2>&1 || { bad "Keycloak ${KC_VERSION} did not start"; docker logs --tail 20 rsl-keycloak; exit 1; }

token() {
    curl -sf -X POST "${KC}/realms/master/protocol/openid-connect/token" -d client_id=admin-cli \
        -d username=admin --data-urlencode "password=${ADMIN_PW}" -d grant_type=password | jq -r .access_token
}
admin() { local m="$1" p="$2"; shift 2; curl -s -o /dev/null -w '%{http_code}' -X "${m}" -H "Authorization: Bearer $(token)" -H 'Content-Type: application/json' "${KC}/admin/realms${p}" "$@"; }
new_realm() {
    admin POST "" -d "{\"realm\":\"$1\",\"enabled\":true}" >/dev/null
    admin POST "/$1/users" -d '{"username":"probe","email":"probe@example.org","enabled":true}' >/dev/null
}
# sends <realm> — has Keycloak mail the realm's one user; prints the login the
# mail server saw, or nothing when no message arrived.
sends() {
    local uid before
    uid="$(curl -s -H "Authorization: Bearer $(token)" "${KC}/admin/realms/$1/users?username=probe&exact=true" | jq -r '.[0].id')"
    before="$(curl -s "${MAIL}/api/v1/messages" | jq .total)"
    admin PUT "/$1/users/${uid}/execute-actions-email" -d '["UPDATE_PASSWORD"]' >/dev/null
    sleep 1
    if [[ "$(curl -s "${MAIL}/api/v1/messages" | jq .total)" -gt "${before}" ]]; then
        curl -s "${MAIL}/api/v1/messages?limit=1" | jq -r '.messages[0].Username'
    fi
}
expect_login() { # <what> <realm> <login>
    local got; got="$(sends "$2")"
    if [[ "${got}" == "$3" ]]; then ok "$1: delivered, logged in as $3"; else bad "$1: wanted a message from login $3, got '${got:-no message}'"; fi
}
set_smtp() { admin PUT "/$1" -d "{\"smtpServer\":$2}" >/dev/null; }

# ── the kernel realm: the installer's settings and the installer's request ──
installer_smtp() ( # <realm> <mode>
    MAIL_SERVICE_MODE="$2"
    gentian_mail_service_mode() { printf '%s' "${MAIL_SERVICE_MODE}"; }
    _derived() { printf '%s' "${KERNEL_PW}"; }
    eval "$(sed -n '/^_keycloak_smtp_settings() {/,/^}/p; /^_keycloak_smtp_configure_shell() {/,/^}/p' scripts/lib/portal-login-bootstrap.sh)"
    KERNEL_DOMAIN=example.org EXTERNAL_SMTP_HOST=relay.example.org
    SMTP_RELAY_USERNAME=relay-user SMTP_RELAY_PASSWORD="${RELAY_PW}"
    _keycloak_smtp_settings || { echo "no settings for mode $2" >&2; exit 1; }
    export REALM="$1" MAIL_SERVICE_MODE SMTP_CONFIGURE=true KEYCLOAK_BASE="${KC}" AUTH
    AUTH="Authorization: Bearer $(token)"
    export SMTP_HOST="${KC_SMTP_HOST}" SMTP_PORT="${KC_SMTP_PORT}" SMTP_USER="${KC_SMTP_USER}" SMTP_PASSWORD="${KC_SMTP_PASSWORD}"
    export SMTP_SSL="${KC_SMTP_SSL}" SMTP_STARTTLS="${KC_SMTP_STARTTLS}" SMTP_FROM="${KC_SMTP_FROM}"
    _keycloak_smtp_configure_shell | sh -eu >/dev/null
)
new_realm kernel-own;   installer_smtp kernel-own system
expect_login "kernel realm, the cluster's own mail" kernel-own "gentian-system@example.org"
new_realm kernel-relay; installer_smtp kernel-relay external
expect_login "kernel realm, a relay" kernel-relay "relay-user"

# ── a tenant realm: the operator's Job script, with the Job's environment ──
job_smtp() ( # <realm> <mode>
    export REALM_SMTP_LAB_REALM="$1" MAIL_SERVICE_MODE="$2" KEYCLOAK_URL="${KC}"
    export KEYCLOAK_ADMIN_USERNAME=admin KEYCLOAK_ADMIN_PASSWORD="${ADMIN_PW}"
    export SMTP_CONFIGURE=true SMTP_PORT=587 SMTP_SSL=false SMTP_STARTTLS=true SMTP_FROM=noreply@example.org SMTP_FROM_NAME="Example Cloud"
    if [[ "$2" == "system" ]]; then
        # keycloak-smtp-credentials holds the kernel realm's login here, and
        # mail-submission-<tenant> the tenant realm's own.
        export SMTP_HOST=mail.example.org SMTP_USER=gentian-system@example.org SMTP_PASSWORD="${KERNEL_PW}"
        export KERNEL_MAIL_HOST=mail.example.org KERNEL_SMTP_USER=noreply@team.example.org KERNEL_SMTP_PASSWORD="${TENANT_PW}"
    else
        export SMTP_HOST=relay.example.org SMTP_USER=relay-user SMTP_PASSWORD="${RELAY_PW}"
    fi
    go test ./internal/controller/ -run 'TestTenantSMTPScriptAgainstKeycloak$' -count=1 >"${W}/job.log" 2>&1 || { cat "${W}/job.log" >&2; exit 1; }
)
new_realm tenant-own;   job_smtp tenant-own system
expect_login "tenant realm, the cluster's own mail" tenant-own "noreply@team.example.org"
new_realm tenant-relay; job_smtp tenant-relay external
expect_login "tenant realm, a relay" tenant-relay "relay-user"

# ── what Keycloak does with a write that leaves the password out ───────────
# These are Keycloak's, not the platform's, and are held here because every
# writer of a realm's mail server has to be written around them: both are
# answered 204 and neither is visible until mail is sent.
smtp_of() { curl -s -H "Authorization: Bearer $(token)" "${KC}/admin/realms/$1" | jq -c "$2"; }
set_smtp tenant-own "$(smtp_of tenant-own '.smtpServer | .fromDisplayName = "Renamed"')"
expect_login "the mask written back with the login unchanged keeps the password" tenant-own "noreply@team.example.org"
set_smtp tenant-own "$(smtp_of tenant-own '.smtpServer | del(.password)')"
if [[ -z "$(sends tenant-own)" ]]; then ok "a mail server written without its password loses it: nothing is sent"
else bad "a mail server written without its password still sends: the writers no longer need to carry it"; fi
job_smtp tenant-own system
expect_login "the Job run again puts the password back" tenant-own "noreply@team.example.org"
set_smtp tenant-own "$(smtp_of tenant-own '.smtpServer | .user = "relay-user"')"
if [[ -z "$(sends tenant-own)" ]]; then ok "the mask beside a changed login drops the password: nothing is sent"
else bad "the mask beside a changed login still sends"; fi

echo ""
if [[ "${fail}" -gt 0 ]]; then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sall %d passed%s\n' "${GREEN}" "${pass}" "${NC}"
