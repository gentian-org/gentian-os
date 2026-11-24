#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-mailbox-exchange-lab.sh
# =============================================================================
# An agent bridge opens a person's mailbox with a token it obtains by
# exchange, and nothing else does (docs/security/design.md §2.16).
#
# The realm is built the way the platform builds it, by hand through the
# Admin API: a sign-in client that names the chaperone's exchange client in
# its tokens' audience, the exchange scope app-<bridge> whose one mapper puts
# the bridge's profile name in the audience, the scope "mailbox" whose one
# mapper puts gentian-dovecot there, and the bridge's own client as apprender
# renders it for a bridge that declares requires.services.mail.imap.tokenSignIn
# (identity_client.go): confidential, no flow but the standard token exchange,
# "mailbox" among its optional scopes. Beside it a second bridge that does not
# declare it.
#
# Then the chain a call takes: the person's token, exchanged by the chaperone
# for the bridge, exchanged by the bridge for "mailbox" -- and every shortcut
# refused. Last, Dovecot itself, with the oauth2 block and the realm's keys the
# mail reconciler writes (dovecotOAuth2Conf, dovecotRealmKeys), checks the
# token locally, signs the person in over IMAP with it, and refuses the token
# one step before it and another person's mailbox.
#
# Needs docker, curl, jq, openssl and python3. Not part of CI: it pulls and
# boots Keycloak and Dovecot.
# =============================================================================
# Each check is a condition check evaluates later, and the functions and
# variables it names are used from there, invisibly to shellcheck.
# shellcheck disable=SC2016,SC2034,SC2329
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

for tool in docker curl jq openssl python3; do
    command -v "${tool}" >/dev/null 2>&1 || { echo "SKIP: ${tool} is not installed"; exit 0; }
done

PINNED="$(awk '/^keycloakx:/{kc=1; next} kc && /^[^ #]/{kc=0} kc && /^  image:/{img=1; next} img && /^  [^ ]/{img=0} img && /^    tag:/{gsub(/"/, "", $2); print $2; exit}' kernel/services/keycloak-idp/manifests/values.yaml)"
[[ -n "${PINNED}" ]] || { echo "No keycloakx.image.tag in kernel/services/keycloak-idp/manifests/values.yaml" >&2; exit 1; }
# The image the chart's Deployment runs.
DOVECOT="$(awk '$1 == "image:" && $2 ~ /^dovecot\// {print $2; exit}' kernel/services/dovecot/manifests/templates/deployment.yaml)"
[[ -n "${DOVECOT}" ]] || { echo "No dovecot image in kernel/services/dovecot/manifests/templates/deployment.yaml" >&2; exit 1; }
PORT="${MAILBOX_LAB_PORT:-28082}"
IMAP_PORT="${MAILBOX_LAB_IMAP_PORT:-28143}"
KC="http://127.0.0.1:${PORT}/auth"
# One issuer whichever address a token is fetched at, as in the cluster: the
# tokens are fetched here, and Dovecot accepts the issuer the realm names.
FRONTEND="https://id.example.org/auth"
NET="mbx-lab"
REALM="lab"
BRIDGE="mail-agent-bridge-ee"
OTHER="other-agent-bridge-ee"
ADMIN_PW="$(openssl rand -hex 12)"
PERSON_PW="$(openssl rand -hex 12)"
EXCHANGER_SECRET="$(openssl rand -hex 16)"
BRIDGE_SECRET="$(openssl rand -hex 16)"
OTHER_SECRET="$(openssl rand -hex 16)"
DOVECOT_SECRET="$(openssl rand -hex 16)"
WORK="$(mktemp -d)"

cleanup() {
    docker rm -f mbx-keycloak mbx-dovecot >/dev/null 2>&1
    docker network rm "${NET}" >/dev/null 2>&1
    rm -rf "${WORK}"
}
trap cleanup EXIT
cleanup 2>/dev/null
WORK="$(mktemp -d)"

docker network create "${NET}" >/dev/null
docker run -d --name mbx-keycloak --network "${NET}" -p "127.0.0.1:${PORT}:8080" \
    -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e "KC_BOOTSTRAP_ADMIN_PASSWORD=${ADMIN_PW}" \
    --entrypoint /opt/keycloak/bin/kc.sh "quay.io/keycloak/keycloak:${PINNED}" \
    start-dev --http-relative-path=/auth --cache=local "--hostname=${FRONTEND}" \
    --hostname-backchannel-dynamic=true --hostname-strict=false --proxy-headers=xforwarded >/dev/null
for _ in $(seq 1 120); do
    curl -sf "${KC}/realms/master" >/dev/null 2>&1 && break
    sleep 2
done
curl -sf "${KC}/realms/master" >/dev/null || { docker logs --tail 30 mbx-keycloak; exit 1; }

admin() { # <method> <path> [json]
    local token
    token="$(curl -sf -X POST "${KC}/realms/master/protocol/openid-connect/token" -d client_id=admin-cli \
        -d username=admin --data-urlencode "password=${ADMIN_PW}" -d grant_type=password | jq -r .access_token)"
    curl -sf -X "$1" -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
        "${KC}/admin/realms$2" ${3:+--data "$3"}
}
client_uuid() { admin GET "/${REALM}/clients?clientId=$1" | jq -r '.[0].id'; }
scope_uuid() { admin GET "/${REALM}/client-scopes" | jq -r --arg n "$1" '.[] | select(.name == $n) | .id'; }
audience_mapper() { # <name> <custom audience> | <name> "" <client audience>
    jq -nc --arg n "$1" --arg c "$2" --arg k "${3:-}" '{name: $n, protocol: "openid-connect",
        protocolMapper: "oidc-audience-mapper", config: ({"id.token.claim": "false", "access.token.claim": "true",
        "introspection.token.claim": "true", "userinfo.token.claim": "false"}
        + (if $c != "" then {"included.custom.audience": $c} else {"included.client.audience": $k} end))}'
}
confidential() { # <clientId> <secret> [extra json]
    jq -nc --arg id "$1" --arg s "$2" --argjson x "${3:-{\}}" '{clientId: $id, secret: $s, enabled: true,
        publicClient: false, standardFlowEnabled: false, implicitFlowEnabled: false,
        directAccessGrantsEnabled: false, serviceAccountsEnabled: false} + $x'
}
exchange_enabled='{"attributes": {"standard.token.exchange.enabled": "true"}}'

# The realm, a person with a mailbox, and the mail server's client.
admin POST "" '{"realm": "lab", "enabled": true}' >/dev/null
admin POST "/${REALM}/users" "$(jq -nc --arg p "${PERSON_PW}" '{username: "ada", email: "ada@example.org",
    emailVerified: true, firstName: "Ada", lastName: "Lab", enabled: true,
    credentials: [{type: "password", value: $p, temporary: false}]}')" >/dev/null
admin POST "/${REALM}/clients" "$(confidential gentian-dovecot "${DOVECOT_SECRET}")" >/dev/null

# The person's sign-in (the zone's client) names the exchange client in aud.
admin POST "/${REALM}/clients" '{"clientId": "zone", "publicClient": true, "directAccessGrantsEnabled": true,
    "standardFlowEnabled": false}' >/dev/null
admin POST "/${REALM}/clients/$(client_uuid zone)/protocol-mappers/models" "$(audience_mapper exchange-audience exchanger)" >/dev/null

# The chaperone's exchange: one client allowed to exchange, one scope per bridge.
admin POST "/${REALM}/clients" "$(confidential exchanger "${EXCHANGER_SECRET}" "$(jq -nc --argjson e "${exchange_enabled}" '$e + {fullScopeAllowed: false}')")" >/dev/null
for b in "${BRIDGE}" "${OTHER}"; do
    admin POST "/${REALM}/client-scopes" "$(jq -nc --arg n "app-${b}" '{name: $n, protocol: "openid-connect",
        attributes: {"include.in.token.scope": "true"}}')" >/dev/null
    admin POST "/${REALM}/client-scopes/$(scope_uuid "app-${b}")/protocol-mappers/models" "$(audience_mapper "app-audience" "${b}")" >/dev/null
    admin PUT "/${REALM}/clients/$(client_uuid exchanger)/optional-client-scopes/$(scope_uuid "app-${b}")" >/dev/null
done

# The scope that opens a mailbox: gentian-dovecot in aud, "mailbox" in scope.
admin POST "/${REALM}/client-scopes" '{"name": "mailbox", "protocol": "openid-connect",
    "attributes": {"include.in.token.scope": "true"}}' >/dev/null
admin POST "/${REALM}/client-scopes/$(scope_uuid mailbox)/protocol-mappers/models" "$(audience_mapper mailbox-audience "" gentian-dovecot)" >/dev/null

# The bridges' own clients, as apprender renders a bridge's: the one that
# declares tokenSignIn has "mailbox" among its optional scopes, the other not.
admin POST "/${REALM}/clients" "$(confidential "${BRIDGE}" "${BRIDGE_SECRET}" "${exchange_enabled}")" >/dev/null
admin POST "/${REALM}/clients" "$(confidential "${OTHER}" "${OTHER_SECRET}" "${exchange_enabled}")" >/dev/null
admin PUT "/${REALM}/clients/$(client_uuid "${BRIDGE}")/optional-client-scopes/$(scope_uuid mailbox)" >/dev/null

TOKEN_URL="${KC}/realms/${REALM}/protocol/openid-connect/token"
exchange() { # <client> <secret> <subject token> <scope> — the whole answer
    curl -s -X POST "${TOKEN_URL}" -u "$1:$2" \
        -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
        --data-urlencode "subject_token=$3" \
        -d subject_token_type=urn:ietf:params:oauth:token-type:access_token \
        -d requested_token_type=urn:ietf:params:oauth:token-type:access_token \
        --data-urlencode "scope=$4"
}
introspect() { # <token> — what Keycloak says of it to gentian-dovecot: the audience the mail server requires
    curl -s -X POST "${TOKEN_URL}/introspect" -u "gentian-dovecot:${DOVECOT_SECRET}" --data-urlencode "token=$1"
}
claims() {
    local p
    p="$(cut -d. -f2 <<<"$1" | tr '_-' '/+')"
    while (( ${#p} % 4 )); do p+="="; done
    base64 -d <<<"${p}" 2>/dev/null
}
aud_has() { claims "$1" | jq -e --arg a "$2" '(.aud | if type == "array" then . else [.] end) | index($a)' >/dev/null; }

echo ""
echo "Keycloak ${PINNED}: the chain of a call"
echo ""

SIGNIN="$(curl -sf -X POST "${TOKEN_URL}" -d client_id=zone -d username=ada \
    --data-urlencode "password=${PERSON_PW}" -d grant_type=password -d scope=openid | jq -r .access_token)"
check "the person signs in" '[[ -n "${SIGNIN}" && "${SIGNIN}" != null ]]'

APP="$(exchange exchanger "${EXCHANGER_SECRET}" "${SIGNIN}" "app-${BRIDGE}" | jq -r '.access_token // empty')"
check "the chaperone exchanges it for the bridge" '[[ -n "${APP}" ]] && aud_has "${APP}" "${BRIDGE}"' \
    "  $(claims "${APP}")"

ANSWER="$(exchange "${BRIDGE}" "${BRIDGE_SECRET}" "${APP}" mailbox)"
MAILBOX="$(jq -r '.access_token // empty' <<<"${ANSWER}")"
check "the bridge exchanges that for mailbox" '[[ -n "${MAILBOX}" ]]' "  ${ANSWER}"
check "  its audience names the mail server" 'aud_has "${MAILBOX}" gentian-dovecot' "  $(claims "${MAILBOX}")"
check "  its scope says mailbox" 'claims "${MAILBOX}" | jq -e ".scope | split(\" \") | index(\"mailbox\")" >/dev/null'
check "  it is the person's, by address" '[[ "$(claims "${MAILBOX}" | jq -r .email)" == ada@example.org ]]' \
    "  $(claims "${MAILBOX}")"
check "  the mail server is told it is active" '[[ "$(introspect "${MAILBOX}" | jq -r .active)" == true ]]'

echo ""
echo "Keycloak ${PINNED}: every shortcut refused"
echo ""

check "the token the bridge is handed opens no mailbox" \
    '[[ "$(introspect "${APP}" | jq -r .active)" != true ]]'
check "the person's sign-in opens no mailbox" \
    '[[ "$(introspect "${SIGNIN}" | jq -r .active)" != true ]]'
check "the bridge cannot exchange a token not made out to it" \
    '[[ -z "$(exchange "${BRIDGE}" "${BRIDGE_SECRET}" "${SIGNIN}" mailbox | jq -r ".access_token // empty")" ]]'
check "another bridge cannot exchange the mail bridge's token" \
    '[[ -z "$(exchange "${OTHER}" "${OTHER_SECRET}" "${APP}" mailbox | jq -r ".access_token // empty")" ]]'
OTHER_APP="$(exchange exchanger "${EXCHANGER_SECRET}" "${SIGNIN}" "app-${OTHER}" | jq -r '.access_token // empty')"
check "a bridge that does not declare it gets no mailbox token" \
    '[[ -n "${OTHER_APP}" && -z "$(exchange "${OTHER}" "${OTHER_SECRET}" "${OTHER_APP}" mailbox | jq -r ".access_token // empty")" ]]'
check "the chaperone's client gets no mailbox token" \
    '[[ -z "$(exchange exchanger "${EXCHANGER_SECRET}" "${SIGNIN}" mailbox | jq -r ".access_token // empty")" ]]'

echo ""
echo "${DOVECOT}: IMAP with the token"
echo ""

# dovecot.conf: the chart's mail layout, reduced to what a sign-in needs, and
# the oauth2 settings and validation keys the mail reconciler writes
# (dovecotOAuth2Conf, dovecotValidationKeys): Dovecot checks the token itself,
# against the realm's published keys, filed under the app the token was
# issued to.
mkdir -p "${WORK}/realms"
cat >"${WORK}/dovecot.conf" <<'CONF'
dovecot_config_version = 2.4.0
dovecot_storage_version = 2.4.0
protocols = imap
listen = *
ssl = no
auth_allow_cleartext = yes
auth_mechanisms = plain
auth_debug = yes
log_path = /dev/stderr
mail_driver = maildir
mail_home = /srv/mail/%{user | domain}/%{user | username}
mail_path = ~
mail_uid = 1000
mail_gid = 1000
first_valid_uid = 1000
userdb static {
  fields {
    uid = 1000
    gid = 1000
    home = /srv/mail/%{user | domain}/%{user | username}
  }
  userdb_static_allow_all_users = yes
}
passdb passwd-file {
  passwd_file_path = /etc/dovecot/users
}
!include_try /etc/dovecot/realms/oauth2.inc
service imap-login {
  inet_listener imap {
    port = 143
  }
}
CONF
: >"${WORK}/users"
cat >"${WORK}/realms/oauth2.inc" <<CONF
auth_mechanisms {
  xoauth2 = yes
  oauthbearer = yes
}
oauth2 {
  introspection_mode = local
  client_id = gentian-dovecot
  client_secret = ${DOVECOT_SECRET}
  username_attribute = email
  scope = mailbox
  issuers = ${FRONTEND}/realms/${REALM}
  oauth2_local_validation {
    dict file {
      dict_file_path = /etc/dovecot/realms/validation-keys
    }
  }
}
CONF
# The realm's signing keys, once for each app that may be given "mailbox":
# the key Dovecot looks up is shared/<the token's azp>/<alg>/<kid>.
curl -sf "${KC}/realms/${REALM}/protocol/openid-connect/certs" \
    | jq -r --arg azp "${BRIDGE}" '.keys[] | select(.use == "sig") | "shared/\($azp)/\(.alg)/\(.kid)", (. | tojson)' \
    >"${WORK}/realms/validation-keys"
chmod -R a+rX "${WORK}"
docker run -d --name mbx-dovecot --network "${NET}" -p "127.0.0.1:${IMAP_PORT}:143" \
    -v "${WORK}/dovecot.conf:/etc/dovecot/dovecot.conf:ro" -v "${WORK}/realms:/etc/dovecot/realms:ro" \
    -v "${WORK}/users:/etc/dovecot/users:ro" \
    "${DOVECOT}" >/dev/null
for _ in $(seq 1 30); do
    python3 -c "import socket; socket.create_connection(('127.0.0.1', ${IMAP_PORT}), 2).close()" 2>/dev/null && break
    sleep 1
done

imap_login() { # <address> <token> — "ok" when Dovecot signs them in
    python3 - "$1" "$2" "${IMAP_PORT}" <<'PY'
import imaplib, sys
address, token, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
imap = imaplib.IMAP4("127.0.0.1", port)
try:
    imap.authenticate("XOAUTH2", lambda _: f"user={address}\x01auth=Bearer {token}\x01\x01".encode())
    imap.select("INBOX")
    print("ok")
except imaplib.IMAP4.error as e:
    print(f"refused: {e}")
finally:
    try:
        imap.logout()
    except Exception:
        pass
PY
}

GOT="$(imap_login ada@example.org "${MAILBOX}")"
check "the mail server signs the person in with the bridge's mailbox token" '[[ "${GOT}" == ok ]]' \
    "  ${GOT}
$(docker logs --tail 20 mbx-dovecot 2>&1)"
GOT="$(imap_login bob@example.org "${MAILBOX}")"
check "and not into another person's mailbox" '[[ "${GOT}" == refused:* ]]' "  ${GOT}"
GOT="$(imap_login ada@example.org "${APP}")"
check "the token the bridge is handed is refused" '[[ "${GOT}" == refused:* ]]' "  ${GOT}"

finish
