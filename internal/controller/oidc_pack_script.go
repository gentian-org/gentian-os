/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

// buildOIDCPackScript provisions Keycloak client scope, mappers,
// client role, group role mapping, and default scopes for one OIDC client.
func buildOIDCPackScript(
	realmName, clientID string,
	pack oidc.Pack,
	templates map[string]oidc.MapperTemplate,
	redirectURIs []string,
	clientSecret string,
	entitlementGroup string,
) string {
	if pack.ServiceClient {
		return buildOIDCServiceClientScript(realmName, clientID, pack)
	}

	redirectJSON, _ := json.Marshal(redirectURIs)
	mapperBlocks := buildMapperPOSTBlocks(pack, templates)

	publicClient := "false"
	if pack.PublicClient {
		publicClient = "true"
	}
	fullScope := "false"
	if pack.FullScopeAllowed {
		fullScope = "true"
	}

	secretClause := ""
	if clientSecret != "" {
		secretClause = `,\"secret\":\"${OIDC_CLIENT_SECRET}\"`
	}

	groupName := entitlementGroup
	if groupName == "" {
		groupName = pack.EntitlementGroup
	}

	scopeLookupBlock := keycloak.ShellLookupClientScopeID()
	clientUUIDBlock := keycloak.ShellRequireID("CLIENT_UUID", "${EXISTING}", "clientId", "${CLIENT_ID}")
	groupIDBlock := keycloak.ShellRequireID("GROUP_ID", "${GROUP_LIST}", "name", "${ENTITLEMENT_GROUP}")

	return keycloak.ShellJSONIDExtractor() + keycloak.ShellScopeIDFromList() + fmt.Sprintf(`set -eu
REALM=%q
CLIENT_ID=%q
SCOPE_NAME=%q
SCOPE_DESC=%q
CLIENT_ROLE=%q
ENTITLEMENT_GROUP=%q
REDIRECT_URIS='%s'
PUBLIC_CLIENT=%s
FULL_SCOPE_ALLOWED=%s

TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

# --- Client scope ---
%s

# --- Protocol mappers on scope ---
%s

# --- OIDC client ---
EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}")
if echo "${EXISTING}" | grep -q '"id"'; then
  echo "client ${CLIENT_ID} already exists"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients" \
    -d "{\"clientId\":\"${CLIENT_ID}\",\"redirectUris\":${REDIRECT_URIS},\"webOrigins\":[\"+\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":${PUBLIC_CLIENT},\"fullScopeAllowed\":${FULL_SCOPE_ALLOWED},\"serviceAccountsEnabled\":false,\"directAccessGrantsEnabled\":false%s}"
  EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}")
  echo "client ${CLIENT_ID} created"
fi
%s
# The client is NOT configured here. app-default composes it, and this Job
# writing the same object is what made it the last one with two writers — the
# shape that let the kernel IdP spend two minutes of every reconcile on the
# wrong first-broker-login flow before anyone noticed.
#
# The create above stays. It is a bootstrap, not ownership: something has to
# make the client before the client role below can hang off it, and this Job is
# waited on in the DataPlane stage while the App claim that composes the client
# is created in AppsAndEdge, the stage after. Whichever gets there first decides
# the initial state and the Composition converges it from then on.
#
# Same division as the realm script and the kernel IdP: create if absent, never
# restate.

# --- Client role ---
ROLE_HTTP=$(curl -s -o /dev/null -w "%%{http_code}" -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}/roles/${CLIENT_ROLE}")
if [ "${ROLE_HTTP}" = "404" ]; then
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}/roles" \
    -d "{\"name\":\"${CLIENT_ROLE}\"}"
  echo "client role ${CLIENT_ROLE} created"
else
  echo "client role ${CLIENT_ROLE} already exists"
fi
ROLE_JSON=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}/roles/${CLIENT_ROLE}")
ROLE_ID=$(echo "${ROLE_JSON}" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)

# --- Map entitlement group to client role ---
GROUP_LIST=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/groups?search=${ENTITLEMENT_GROUP}")
%s
curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/groups/${GROUP_ID}/role-mappings/clients/${CLIENT_UUID}" \
  -d "[{\"id\":\"${ROLE_ID}\",\"name\":\"${CLIENT_ROLE}\"}]" >/dev/null || true
echo "group ${ENTITLEMENT_GROUP} mapped to client role ${CLIENT_ROLE}"

# The default client scopes are NOT attached here. app-default composes a
# ClientDefaultScopes for exactly the same six — profile, email, roles,
# web-origins, acr and the pack's own scope — and it reports them all attached.
# This loop was the second place this Job wrote an object the Composition owns.
#
# It also swallowed its own failures, so a scope that never attached looked
# identical to one that did.

echo "oidc pack ${CLIENT_ID} provisioned in realm ${REALM}"`,
		realmName, clientID, pack.ScopeName, pack.ScopeDescription, pack.ClientRole, groupName,
		string(redirectJSON), publicClient, fullScope,
		// One secretClause, not two: the second filled the client PUT that this
		// Job no longer makes.
		scopeLookupBlock, mapperBlocks, secretClause, clientUUIDBlock, groupIDBlock)
}

// buildOIDCServiceClientScript provisions only a confidential client.
//
// Kernel Dovecot is the case this exists for: it calls the realm's token
// introspection endpoint to validate XOAUTH2 access tokens that OTHER clients
// issued, so all it needs is credentials it can authenticate with. It is never
// redirected to, so standardFlowEnabled is false and there are no redirect URIs;
// nobody is granted access TO it, so there is no client scope, client role or
// entitlement group. Running the app-shaped script for it would leave an unused
// scope and an empty role in every tenant realm and suggest, in the Keycloak
// admin UI, that users can be entitled to a mail server.
//
// The client secret comes from the OIDC_CLIENT_SECRET env the Job carries, never
// from an argument, so it stays out of the rendered script and out of Job specs.
func buildOIDCServiceClientScript(realmName, clientID string, pack oidc.Pack) string {
	fullScope := "false"
	if pack.FullScopeAllowed {
		fullScope = "true"
	}
	clientUUIDBlock := keycloak.ShellRequireID("CLIENT_UUID", "${EXISTING}", "clientId", "${CLIENT_ID}")

	// Written on both create and update: a client that already exists from an
	// earlier release may predate serviceClient and still carry the browser flow.
	body := `{\"clientId\":\"${CLIENT_ID}\",\"protocol\":\"openid-connect\",\"publicClient\":false,` +
		`\"standardFlowEnabled\":false,\"implicitFlowEnabled\":false,\"directAccessGrantsEnabled\":false,` +
		`\"serviceAccountsEnabled\":false,\"redirectUris\":[],\"webOrigins\":[],` +
		`\"fullScopeAllowed\":` + fullScope + `,\"secret\":\"${OIDC_CLIENT_SECRET}\"}`

	return keycloak.ShellJSONIDExtractor() + fmt.Sprintf(`set -eu
REALM=%q
CLIENT_ID=%q

if [ -z "${OIDC_CLIENT_SECRET:-}" ]; then
  echo "ERROR: OIDC_CLIENT_SECRET is empty; a confidential service client cannot be provisioned without it" >&2
  exit 1
fi

TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}")
if echo "${EXISTING}" | grep -q '"id"'; then
  echo "service client ${CLIENT_ID} already exists in realm ${REALM}"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients" \
    -d "%s"
  EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}")
  echo "service client ${CLIENT_ID} created in realm ${REALM}"
fi
%s
curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}" \
  -d "%s"

# Prove the credentials work now, rather than discovering at the first IMAP login
# that introspection returns 401. An access token is not needed for this: the
# introspection endpoint authenticates the CALLER first, so a syntactically valid
# but meaningless token still distinguishes "client cannot authenticate" (401)
# from "token is not active" (200 with active=false).
PROBE=$(curl -s -o /dev/null -w "%%{http_code}" \
  -X POST "${KEYCLOAK_URL}/realms/${REALM}/protocol/openid-connect/token/introspect" \
  -u "${CLIENT_ID}:${OIDC_CLIENT_SECRET}" \
  -d "token=probe")
if [ "${PROBE}" = "401" ] || [ "${PROBE}" = "403" ]; then
  echo "ERROR: ${CLIENT_ID} cannot authenticate to introspection in realm ${REALM} (HTTP ${PROBE})" >&2
  exit 1
fi
echo "service client ${CLIENT_ID} can introspect in realm ${REALM} (HTTP ${PROBE})"`,
		realmName, clientID, body, clientUUIDBlock, body)
}

func buildMapperPOSTBlocks(pack oidc.Pack, templates map[string]oidc.MapperTemplate) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
# Drop corrupt mappers left by earlier failed provision runs.
MAPPERS=$(curl -sS -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${SCOPE_UUID}/protocol-mappers/models" 2>/dev/null || echo "[]")
for _kj_mid in $(printf '%%s' "${MAPPERS}" | jq -r '.[] | select(.name=="oidc-usermodel-attribute-mapper") | .id // empty' 2>/dev/null); do
  [ -z "${_kj_mid}" ] && continue
  curl -sS -X DELETE -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${SCOPE_UUID}/protocol-mappers/models/${_kj_mid}" >/dev/null 2>&1 || true
  echo "removed corrupt mapper id=${_kj_mid} from scope ${SCOPE_NAME}"
done
`)
	// No mapper POSTs. app-default composes a ProtocolMapper per entry in
	// pack.Mappers, resolved through the catalogue's mapperTemplates, and those
	// adopted the live mappers by their Keycloak ids rather than creating new
	// ones — verified on corp, where all three kept their ids and their config.
	//
	// The corrupt-mapper cleanup above stays. It deletes mappers whose *name* is
	// literally "oidc-usermodel-attribute-mapper", left by a much older failed
	// run, and nothing declarative covers that.
	return b.String()
}

// buildOIDCBrowserFlowScript makes the tenant realm authenticate its own users.
//
// It used to do the opposite: it built a custom "browser-kernel-idp" flow of
// Cookie then identity-provider-redirector with defaultProvider=kernel, and bound
// the realm to it. That left the tenant realm with no credential form at all, so
// it could only recognise an existing cookie or bounce to the kernel realm — and
// every interactive login in the system, including for apps that live in the
// tenant realm, ended up rendering the kernel realm's form.
//
// Tenant members live in the tenant realm and every per-app OIDC client is
// registered there, so that is where the session belongs. Keycloak's stock
// "browser" flow already does what is wanted — Cookie first, then forms — and its
// identity-provider-redirector stays inert without a default provider. So rather
// than build another custom flow, this binds the realm back to the built-in one
// and removes the redirect flow if an earlier reconcile created it.
func buildOIDCBrowserFlowScript(realmName string) string {
	return fmt.Sprintf(`set -eu
REALM=%q
LEGACY_FLOW="browser-kernel-idp"
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

# Bind first, unbind second: Keycloak refuses to delete a flow the realm still
# uses, and a realm pointing at a flow that is about to be deleted would be a
# window with no usable login at all.
REALM_JSON=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}")
CURRENT=$(printf '%%s' "${REALM_JSON}" | jq -r '.browserFlow')
if [ "${CURRENT}" = "browser" ]; then
  echo "realm ${REALM} already uses the built-in browser flow"
else
  curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}" \
    -d '{"browserFlow":"browser"}' >/dev/null
  echo "realm ${REALM} browser flow set to the built-in browser flow (was ${CURRENT})"
fi

# The tenant realm renders its own login form now, so it needs the Gentian theme.
curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}" \
  -d '{"loginTheme":"gentian"}' >/dev/null
echo "realm ${REALM} login theme set to gentian"

FLOWS=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows")
FLOW_ID=$(printf '%%s' "${FLOWS}" | jq -r --arg a "${LEGACY_FLOW}" '.[] | select(.alias==$a) | .id // empty')
if [ -n "${FLOW_ID}" ]; then
  if curl -sf -X DELETE -H "${AUTH_HEADER}" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ID}" >/dev/null 2>&1; then
    echo "removed the legacy ${LEGACY_FLOW} flow"
  else
    # Not fatal: the realm is already on the built-in flow, so an orphaned
    # definition changes no behaviour. Still worth reporting.
    echo "WARNING: could not delete the legacy ${LEGACY_FLOW} flow; it is unused but still defined" >&2
  fi
fi`, realmName)
}

func buildEnsureFirstBrokerLoginFlowShell(realmExpr string) string {
	return buildEnsureFirstBrokerLoginFlowShellWithAlias(realmExpr, firstBrokerLoginFlowAlias)
}

// buildEnsureFirstBrokerLoginFlowShellWithAlias is like buildEnsureFirstBrokerLoginFlowShell
// but allows a custom flow alias (e.g. kernel portal broker login).
func buildEnsureFirstBrokerLoginFlowShellWithAlias(realmExpr, flowAlias string) string {
	return fmt.Sprintf(`
REALM=%s
FLOW_ALIAS=%q
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

FLOWS=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows")
if echo "${FLOWS}" | grep -Fq "\"alias\":\"${FLOW_ALIAS}\""; then
  echo "first broker login flow ${FLOW_ALIAS} already exists"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows" \
    -d "{\"alias\":\"${FLOW_ALIAS}\",\"description\":\"Confirm/verify link kernel IdP to tenant users by email\",\"providerId\":\"basic-flow\",\"topLevel\":true,\"builtIn\":false}"
  echo "first broker login flow ${FLOW_ALIAS} created"
fi

for PROVIDER in idp-detect-existing-broker-user idp-confirm-link idp-email-verification; do
  EXEC_ID=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions" \
    | jq -r --arg p "${PROVIDER}" 'map(select(.providerId == $p))[0].id // empty')
  if [ -z "${EXEC_ID}" ]; then
    curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions/execution" \
      -d "{\"provider\":\"${PROVIDER}\",\"requirement\":\"REQUIRED\"}"
    EXEC_ID=$(curl -sf -H "${AUTH_HEADER}" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions" \
      | jq -r --arg p "${PROVIDER}" 'map(select(.providerId == $p))[0].id // empty')
    echo "added ${PROVIDER} to ${FLOW_ALIAS}"
  fi
  if [ -n "${EXEC_ID}" ]; then
    curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions" \
      -d "{\"id\":\"${EXEC_ID}\",\"requirement\":\"REQUIRED\"}"
  fi
done
echo "first broker login flow ${FLOW_ALIAS} ready (detect + confirm-link + email-verification)"`, realmExpr, flowAlias)
}

// buildFirstBrokerLoginFlowScript configures a tenant-realm first-broker-login flow
// that links kernel IdP identities to existing tenant users by email with confirmation.
// See Keycloak docs: "Detect existing user first login flow".
func buildFirstBrokerLoginFlowScript(realmName string) string {
	return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
%s

# Drop stale kernel IdP links left from the old confirm/re-auth flow or partial
# links. Users re-link silently on the next broker login via auto-link.
PAGE=0
while true; do
  USERS=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/users?first=${PAGE}&max=100" || echo "[]")
  COUNT=$(printf '%%s' "${USERS}" | jq 'length')
  if [ "${COUNT}" -eq 0 ]; then
    break
  fi
  printf '%%s' "${USERS}" | jq -r '.[].id' | while read -r UID; do
    [ -z "${UID}" ] && continue
    HTTP=$(curl -s -o /dev/null -w "%%{http_code}" -X DELETE -H "${AUTH_HEADER}" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/users/${UID}/federated-identity/kernel")
    if [ "${HTTP}" = "204" ]; then
      echo "removed stale kernel broker link for user ${UID}"
    fi
  done
  PAGE=$((PAGE + 100))
  if [ "${COUNT}" -lt 100 ]; then
    break
  fi
done
echo "kernel broker link purge finished for realm ${REALM}"`, buildEnsureFirstBrokerLoginFlowShell(fmt.Sprintf("%q", realmName)))
}
