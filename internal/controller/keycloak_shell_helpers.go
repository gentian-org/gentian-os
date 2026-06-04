// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"
	"regexp"
	"strings"
)

// keycloakShellJSONIDExtractor returns a POSIX sh helper that extracts the "id"
// field from minified Keycloak Admin API JSON arrays by matching another field
// (e.g. name=ldap, clientId=broker-demo). Objects are split on "},{" so fields
// between "name" and "id" (description, protocol, …) do not break extraction.
func keycloakShellJSONIDExtractor() string {
	return `keycloak_json_id_by_attr() {
  _kj_json="$1"
  _kj_attr="$2"
  _kj_val="$3"
  _kj_id=$(printf '%s' "${_kj_json}" | sed 's/},{/}\n{/g' | grep -F "\"${_kj_attr}\":\"${_kj_val}\"" | head -1 | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
}
`
}

// keycloakShellRequireID emits shell that assigns outVar from the extractor or exits 1.
func keycloakShellRequireID(outVar, jsonVar, attr, value string) string {
	return fmt.Sprintf(`keycloak_json_id_by_attr %s "%s" "%s"
%s="${_kj_id}"
if [ -z "${%s}" ]; then
  echo "ERROR: could not resolve Keycloak resource id (%s=%s)" >&2
  exit 1
fi`, jsonVar, attr, value, outVar, outVar, attr, value)
}

// extractKeycloakJSONIDByAttr mirrors the shell object-split logic for unit tests.
func extractKeycloakJSONIDByAttr(json, attr, value string) string {
	needle := `"` + attr + `":"` + value + `"`
	idRe := regexp.MustCompile(`"id":"([^"]+)"`)
	inner := strings.TrimSpace(json)
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
