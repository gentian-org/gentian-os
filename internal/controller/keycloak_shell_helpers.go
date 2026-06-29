// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// keycloakProvisionerBootstrap installs curl and jq before provisioner scripts run.
const keycloakProvisionerBootstrap = "apk add --no-cache --quiet curl jq >/dev/null 2>&1 || { echo 'ERROR: install curl/jq failed' >&2; exit 1; }; "

// keycloakShellJSONIDExtractor returns a POSIX sh helper that extracts the "id"
// field from Keycloak Admin API JSON arrays. jq is preferred; sed/grep fallbacks
// handle minified arrays and objects with fields between name and id.
func keycloakShellJSONIDExtractor() string {
	return `keycloak_json_id_by_attr() {
  _kj_json="$1"
  _kj_attr="$2"
  _kj_val="$3"
  _kj_id=""
  if command -v jq >/dev/null 2>&1; then
    _kj_id=$(printf '%s' "${_kj_json}" | jq -r --arg a "${_kj_attr}" --arg v "${_kj_val}" '
      (if type == "array" then .[] elif (.content? | type) == "array" then .content[] else empty end)
      | select(has($a) and (.[$a] | tostring) == $v and has("id") and (.id | type) == "string" and .id != "")
      | .id // empty' 2>/dev/null | head -1)
    if [ "${_kj_id}" = "null" ]; then
      _kj_id=""
    fi
  fi
  if [ -z "${_kj_id}" ]; then
    _kj_flat=$(printf '%s' "${_kj_json}" | tr -d '\n\r')
    _kj_id=$(printf '%s' "${_kj_flat}" | sed 's/},[[:space:]]*{/}\n{/g' | grep -F "\"${_kj_attr}\":\"${_kj_val}\"" | head -1 | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
  fi
}
`
}

// keycloakShellWaitForRealm polls until the tenant realm exists. Crossplane applies
// all provisioning Jobs from jobs.json at once; dependent Jobs must wait for
// keycloak-realm-* before calling realm-scoped Admin API endpoints.
func keycloakShellWaitForRealm(realmExpr string) string {
	return fmt.Sprintf(`
_wait_realm=0
while [ ${_wait_realm} -lt 90 ]; do
  if curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/%s" >/dev/null 2>&1; then
    break
  fi
  _wait_realm=$((_wait_realm + 1))
  sleep 2
done
if ! curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/%s" >/dev/null 2>&1; then
  echo "ERROR: realm %s not available after waiting" >&2
  exit 1
fi
`, realmExpr, realmExpr, realmExpr)
}

// keycloakShellRequireID emits shell that assigns outVar from the extractor or exits 1.
// jsonVar must be a shell expansion such as ${EXISTING}; it is always double-quoted
// so JSON arrays/objects are not word-split by the shell.
func keycloakShellRequireID(outVar, jsonVar, attr, value string) string {
	return fmt.Sprintf(`keycloak_json_id_by_attr "%s" "%s" "%s"
%s="${_kj_id}"
if [ -z "${%s}" ]; then
  echo "ERROR: could not resolve Keycloak resource id (%s=%s)" >&2
  exit 1
fi`, jsonVar, attr, value, outVar, outVar, attr, value)
}

// keycloakShellScopeIDFromList defines a helper to extract a client-scope id by name from JSON.
func keycloakShellScopeIDFromList() string {
	return `_kj_scope_id_from_list() {
  _kj_id=$(printf '%s' "$1" | jq -r --arg n "${SCOPE_NAME}" '
    (if type == "array" then .[] elif (.content? | type) == "array" then .content[] else empty end)
    | select(.name? == $n) | .id // empty' 2>/dev/null | head -1)
  if [ "${_kj_id}" = "null" ]; then
    _kj_id=""
  fi
}
`
}

// keycloakShellLookupClientScopeID resolves SCOPE_UUID for the OIDC pack job (create if missing).
func keycloakShellLookupClientScopeID() string {
	return keycloakShellScopeIDFromList() + `
SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
_kj_scope_id_from_list "${SCOPE_LIST}"
SCOPE_UUID="${_kj_id}"
if [ -z "${SCOPE_UUID}" ]; then
  for _kj_ep in default-default-client-scopes optional-client-scopes; do
    _kj_list=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/${_kj_ep}" 2>/dev/null || true)
    if [ -n "${_kj_list}" ]; then
      _kj_scope_id_from_list "${_kj_list}"
      SCOPE_UUID="${_kj_id}"
      if [ -n "${SCOPE_UUID}" ]; then
        break
      fi
    fi
  done
fi
if [ -z "${SCOPE_UUID}" ]; then
  _kj_hdr=$(curl -si -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes" \
    -d "{\"name\":\"${SCOPE_NAME}\",\"description\":\"${SCOPE_DESC}\",\"protocol\":\"openid-connect\"}")
  SCOPE_UUID=$(echo "${_kj_hdr}" | grep -i '^Location:' | tail -1 | tr -d '\r' | sed 's|.*/||')
  if [ -n "${SCOPE_UUID}" ]; then
    echo "client scope ${SCOPE_NAME} created"
  else
    echo "client scope ${SCOPE_NAME} already exists"
    SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
    _kj_scope_id_from_list "${SCOPE_LIST}"
    SCOPE_UUID="${_kj_id}"
  fi
else
  echo "client scope ${SCOPE_NAME} already exists"
fi
if [ -z "${SCOPE_UUID}" ]; then
  echo "ERROR: could not resolve client scope id (name=${SCOPE_NAME})" >&2
  exit 1
fi
echo "resolved client scope ${SCOPE_NAME} id=${SCOPE_UUID}"
`
}

// extractKeycloakJSONIDByAttr mirrors the shell logic for unit tests (jq when available).
func extractKeycloakJSONIDByAttr(raw, attr, value string) string {
	var walk func(any)
	var found string
	walk = func(v any) {
		if found != "" {
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		if fmt.Sprint(m[attr]) == value {
			if id, ok := m["id"].(string); ok && id != "" {
				found = id
				return
			}
		}
		for _, child := range m {
			walk(child)
		}
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" {
		var parsed any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			if arr, ok := parsed.([]any); ok {
				for _, item := range arr {
					walk(item)
				}
			} else {
				walk(parsed)
			}
			if found != "" {
				return found
			}
		}
	}
	// sed/grep fallback (same as shell)
	needle := `"` + attr + `":"` + value + `"`
	idRe := regexp.MustCompile(`"id":"([^"]+)"`)
	inner := strings.TrimSpace(raw)
	inner = strings.TrimPrefix(inner, "[")
	inner = strings.TrimSuffix(inner, "]")
	if inner == "" {
		return ""
	}
	for _, obj := range strings.Split(inner, "},{") {
		if !strings.Contains(obj, needle) {
			continue
		}
		if m := idRe.FindStringSubmatch(obj); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}
