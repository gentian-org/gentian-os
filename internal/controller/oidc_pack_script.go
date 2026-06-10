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

	scopeLookupBlock := keycloakShellLookupClientScopeID()
	clientUUIDBlock := keycloakShellRequireID("CLIENT_UUID", "${EXISTING}", "clientId", "${CLIENT_ID}")
	groupIDBlock := keycloakShellRequireID("GROUP_ID", "${GROUP_LIST}", "name", "${LDAP_GROUP}")

	return keycloakShellJSONIDExtractor() + keycloakShellScopeIDFromList() + fmt.Sprintf(`set -eu
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
curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}" \
  -d "{\"clientId\":\"${CLIENT_ID}\",\"redirectUris\":${REDIRECT_URIS},\"webOrigins\":[\"+\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":${PUBLIC_CLIENT},\"fullScopeAllowed\":${FULL_SCOPE_ALLOWED},\"serviceAccountsEnabled\":false,\"directAccessGrantsEnabled\":false%s}"
echo "client ${CLIENT_ID} configured"

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
%s
curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/groups/${GROUP_ID}/role-mappings/clients/${CLIENT_UUID}" \
  -d "[{\"id\":\"${ROLE_ID}\",\"name\":\"${CLIENT_ROLE}\"}]" >/dev/null || true
echo "group ${LDAP_GROUP} mapped to client role ${CLIENT_ROLE}"

# --- Default client scopes (built-ins + app scope) ---
for SCOPE in profile email roles web-origins acr ${SCOPE_NAME}; do
  keycloak_json_id_by_attr "${SCOPE_LIST}" "name" "${SCOPE}"
  SID="${_kj_id}"
  if [ -n "${SID}" ]; then
    curl -sf -X PUT -H "${AUTH_HEADER}" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_UUID}/default-client-scopes/${SID}" >/dev/null 2>&1 || true
  fi
done
SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")

echo "oidc pack ${CLIENT_ID} provisioned in realm ${REALM}"`,
		realmName, clientID, pack.ScopeName, pack.ScopeDescription, pack.ClientRole, pack.LDAPGroup,
		string(redirectJSON), publicClient, fullScope,
		scopeLookupBlock, mapperBlocks, secretClause, clientUUIDBlock, secretClause, groupIDBlock)
}

type protocolMapperPOST struct {
	Name            string            `json:"name"`
	Protocol        string            `json:"protocol"`
	ProtocolMapper  string            `json:"protocolMapper"`
	ConsentRequired bool              `json:"consentRequired"`
	Config          map[string]string `json:"config"`
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
	for _, templateKey := range pack.Mappers {
		tmpl, ok := templates[templateKey]
		if !ok {
			continue
		}
		mapperName := templateKey
		if tmpl.KeycloakName != "" {
			mapperName = tmpl.KeycloakName
		}
		cfg := make(map[string]string, len(tmpl.Config)+1)
		for k, v := range tmpl.Config {
			cfg[k] = v
		}
		if tmpl.ProtocolMapper == "oidc-usermodel-attribute-mapper" {
			if _, ok := cfg["multivalued"]; !ok {
				cfg["multivalued"] = "false"
			}
		}
		bodyJSON, err := json.Marshal(protocolMapperPOST{
			Name:            mapperName,
			Protocol:        "openid-connect",
			ProtocolMapper:  tmpl.ProtocolMapper,
			ConsentRequired: false,
			Config:          cfg,
		})
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, `
MAPPERS=$(curl -sS -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${SCOPE_UUID}/protocol-mappers/models" 2>/dev/null || echo "[]")
if echo "${MAPPERS}" | grep -Fq "\"name\":\"%s\""; then
  echo "mapper %s already on scope ${SCOPE_NAME}"
else
  cat > "/tmp/mapper-%s.json" <<'EOF'
%s
EOF
  _kj_mbody=$(mktemp)
  _kj_mh=$(curl -sS -o "${_kj_mbody}" -w "%%{http_code}" -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${SCOPE_UUID}/protocol-mappers/models" \
    -d @/tmp/mapper-%s.json)
  rm -f "/tmp/mapper-%s.json"
  if [ "${_kj_mh}" != "201" ] && [ "${_kj_mh}" != "409" ]; then
    echo "ERROR: mapper %s POST failed (HTTP ${_kj_mh}): $(cat "${_kj_mbody}" 2>/dev/null)" >&2
    rm -f "${_kj_mbody}"
    exit 1
  fi
  rm -f "${_kj_mbody}"
  echo "mapper %s added to scope ${SCOPE_NAME}"
fi`, mapperName, mapperName, templateKey, string(bodyJSON), templateKey, templateKey, mapperName, mapperName)
	}
	return b.String()
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
  | jq -r 'map(select(.providerId == "identity-provider-redirector"))[0].id // empty')
if [ -z "${EXEC_ID}" ]; then
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions/execution" \
    -d "{\"provider\":\"identity-provider-redirector\",\"requirement\":\"REQUIRED\"}"
  EXEC_ID=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions" \
    | jq -r 'map(select(.providerId == "identity-provider-redirector"))[0].id // empty')
fi
if [ -n "${EXEC_ID}" ]; then
  curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows/${FLOW_ALIAS}/executions" \
    -d "{\"id\":\"${EXEC_ID}\",\"requirement\":\"REQUIRED\"}"
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/executions/${EXEC_ID}/config" \
    -d "{\"alias\":\"autoredirect-kernel\",\"config\":{\"defaultProvider\":\"kernel\"}}" >/dev/null 2>&1 || true
  echo "identity-provider-redirector execution ${EXEC_ID} set to REQUIRED (defaultProvider=kernel)"
fi

curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}" \
  -d "{\"browserFlow\":\"${FLOW_ALIAS}\"}" >/dev/null
echo "realm ${REALM} browser flow set to ${FLOW_ALIAS}"`, realmName)
}

// buildFirstBrokerLoginFlowScript configures a tenant-realm first-broker-login flow
// that links kernel IdP identities to existing LDAP users by email without prompting.
// See Keycloak docs: "Detect existing user first login flow".
func buildFirstBrokerLoginFlowScript(realmName string) string {
	return fmt.Sprintf(`set -eu
REALM=%q
FLOW_ALIAS=%q
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

FLOWS=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows")
if echo "${FLOWS}" | grep -Fq "\"alias\":\"${FLOW_ALIAS}\""; then
  echo "first broker login flow ${FLOW_ALIAS} already exists"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/authentication/flows" \
    -d "{\"alias\":\"${FLOW_ALIAS}\",\"description\":\"Auto-link kernel IdP to LDAP users by email\",\"providerId\":\"basic-flow\",\"topLevel\":true,\"builtIn\":false}"
  echo "first broker login flow ${FLOW_ALIAS} created"
fi

for PROVIDER in idp-detect-existing-broker-user idp-auto-link; do
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
echo "first broker login flow ${FLOW_ALIAS} ready (detect + auto-link)"`, realmName, firstBrokerLoginFlowAlias)
}
