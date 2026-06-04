// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gentian-org/gentian-os/internal/oidc"
)

// buildOIDCPackScript provisions OpenDesk-style Keycloak client scope, mappers,
// client role, group role mapping, and default scopes for one OIDC client.
func buildOIDCPackScript(
	realmName, clientID string,
	pack oidc.Pack,
	templates map[string]oidc.MapperTemplate,
	redirectURIs []string,
	clientSecret string,
) string {
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

	return fmt.Sprintf(`set -eu
REALM=%q
CLIENT_ID=%q
SCOPE_NAME=%q
SCOPE_DESC=%q
CLIENT_ROLE=%q
LDAP_GROUP=%q
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
SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
if echo "${SCOPE_LIST}" | grep -Fq "\"name\":\"${SCOPE_NAME}\""; then
  SCOPE_UUID=$(echo "${SCOPE_LIST}" | tr ',' '\n' | grep -F "\"name\":\"${SCOPE_NAME}\"" | head -1 | sed 's/.*"id":"\([^"]*\)".*/\1/')
  echo "client scope ${SCOPE_NAME} already exists"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes" \
    -d "{\"name\":\"${SCOPE_NAME}\",\"description\":\"${SCOPE_DESC}\",\"protocol\":\"openid-connect\"}"
  SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
  SCOPE_UUID=$(echo "${SCOPE_LIST}" | tr ',' '\n' | grep -F "\"name\":\"${SCOPE_NAME}\"" | head -1 | sed 's/.*"id":"\([^"]*\)".*/\1/')
  echo "client scope ${SCOPE_NAME} created"
fi

# --- Protocol mappers on scope ---
%s

# --- OIDC client ---
EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}")
if echo "${EXISTING}" | grep -q '"id"'; then
  CLIENT_UUID=$(echo "${EXISTING}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
  curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}" \
    -d "{\"clientId\":\"${CLIENT_ID}\",\"redirectUris\":${REDIRECT_URIS},\"webOrigins\":[\"+\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":${PUBLIC_CLIENT},\"fullScopeAllowed\":${FULL_SCOPE_ALLOWED},\"serviceAccountsEnabled\":false,\"directAccessGrantsEnabled\":false%s}"
  echo "client ${CLIENT_ID} updated"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients" \
    -d "{\"clientId\":\"${CLIENT_ID}\",\"redirectUris\":${REDIRECT_URIS},\"webOrigins\":[\"+\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":${PUBLIC_CLIENT},\"fullScopeAllowed\":${FULL_SCOPE_ALLOWED},\"serviceAccountsEnabled\":false,\"directAccessGrantsEnabled\":false%s}"
  EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}")
  CLIENT_UUID=$(echo "${EXISTING}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
  echo "client ${CLIENT_ID} created"
fi

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

# --- Map LDAP group to client role ---
GROUP_LIST=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/groups?search=${LDAP_GROUP}")
GROUP_ID=$(echo "${GROUP_LIST}" | tr ',' '\n' | grep -F "\"name\":\"${LDAP_GROUP}\"" | head -1 | sed 's/.*"id":"\([^"]*\)".*/\1/')
if [ -z "${GROUP_ID}" ]; then
  echo "LDAP group ${LDAP_GROUP} not found in Keycloak realm ${REALM}; ensure LDAP group mapper ran" >&2
  exit 1
fi
curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/groups/${GROUP_ID}/role-mappings/clients/${CLIENT_UUID}" \
  -d "[{\"id\":\"${ROLE_ID}\",\"name\":\"${CLIENT_ROLE}\"}]" >/dev/null || true
echo "group ${LDAP_GROUP} mapped to client role ${CLIENT_ROLE}"

# --- Default client scopes (built-ins + app scope) ---
for SCOPE in profile email roles web-origins acr ${SCOPE_NAME}; do
  SID=$(echo "${SCOPE_LIST}" | tr ',' '\n' | grep -F "\"name\":\"${SCOPE}\"" | head -1 | sed 's/.*"id":"\([^"]*\)".*/\1/')
  if [ -n "${SID}" ]; then
    curl -sf -X PUT -H "${AUTH_HEADER}" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}/default-client-scopes/${SID}" >/dev/null 2>&1 || true
  fi
done
SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")

echo "oidc pack ${CLIENT_ID} provisioned in realm ${REALM}"`,
		realmName, clientID, pack.ScopeName, pack.ScopeDescription, pack.ClientRole, pack.LDAPGroup,
		string(redirectJSON), publicClient, fullScope,
		mapperBlocks, secretClause, secretClause)
}

func buildMapperPOSTBlocks(pack oidc.Pack, templates map[string]oidc.MapperTemplate) string {
	var b strings.Builder
	for _, name := range pack.Mappers {
		tmpl, ok := templates[name]
		if !ok {
			continue
		}
		cfgJSON := mapperConfigJSON(tmpl.Config)
		fmt.Fprintf(&b, `
MAPPERS=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${SCOPE_UUID}/protocol-mappers/models")
if echo "${MAPPERS}" | grep -Fq "\"name\":\"%s\""; then
  echo "mapper %s already on scope ${SCOPE_NAME}"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${SCOPE_UUID}/protocol-mappers/models" \
    -d "{\"name\":%q,\"protocol\":\"openid-connect\",\"protocolMapper\":%q,\"config\":%s}"
  echo "mapper %s added to scope ${SCOPE_NAME}"
fi`, name, name, name, tmpl.ProtocolMapper, cfgJSON, name)
	}
	return b.String()
}

func mapperConfigJSON(cfg map[string]string) string {
	inner := make([]string, 0, len(cfg))
	for k, v := range cfg {
		inner = append(inner, fmt.Sprintf("%q:[%q]", k, v))
	}
	return "{" + strings.Join(inner, ",") + "}"
}

// buildOIDCBrowserFlowScript configures the tenant realm browser flow to auto-redirect to the kernel IdP.
func buildOIDCBrowserFlowScript(realmName string) string {
	return fmt.Sprintf(`set -eu
REALM=%q
FLOW_ALIAS="browser-kernel-idp"
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

FLOWS=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows")
if echo "${FLOWS}" | grep -Fq "\"alias\":\"${FLOW_ALIAS}\""; then
  echo "browser flow ${FLOW_ALIAS} already exists"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows" \
    -d "{\"alias\":\"${FLOW_ALIAS}\",\"description\":\"Auto-redirect to kernel IdP\",\"providerId\":\"basic-flow\",\"topLevel\":true,\"builtIn\":false}"
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions/execution" \
    -d "{\"provider\":\"identity-provider-redirector\",\"requirement\":\"REQUIRED\"}"
  echo "browser flow ${FLOW_ALIAS} created"
fi

EXEC_ID=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions" \
  | tr ',' '\n' | grep -F '"providerId":"identity-provider-redirector"' | head -1 | sed 's/.*"id":"\([^"]*\)".*/\1/')
if [ -n "${EXEC_ID}" ]; then
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/executions/${EXEC_ID}/config" \
    -d "{\"alias\":\"autoredirect-kernel\",\"config\":{\"defaultProvider\":\"kernel\"}}" >/dev/null 2>&1 || true
fi

curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}" \
  -d "{\"browserFlow\":\"${FLOW_ALIAS}\"}" >/dev/null
echo "realm ${REALM} browser flow set to ${FLOW_ALIAS}"`, realmName)
}
