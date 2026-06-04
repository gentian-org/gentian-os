// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"
	"regexp"
)

// keycloakShellJSONIDExtractor returns a POSIX sh helper that extracts the "id"
// field from minified Keycloak Admin API JSON arrays by matching another field
// (e.g. name=ldap, clientId=broker-demo). Keycloak usually emits "id" before the
// matched attribute; a fallback pattern handles the reverse order.
func keycloakShellJSONIDExtractor() string {
	return `keycloak_json_id_by_attr() {
  _kj_json="$1"
  _kj_attr="$2"
  _kj_val="$3"
  _kj_id=$(printf '%s' "${_kj_json}" | sed -n "s/.*\"id\":\"\([^\"]*\)\",\"${_kj_attr}\":\"${_kj_val}\".*/\1/p" | head -1)
  if [ -z "${_kj_id}" ]; then
    _kj_id=$(printf '%s' "${_kj_json}" | sed -n "s/.*\"${_kj_attr}\":\"${_kj_val}\",\"id\":\"\([^\"]*\)\".*/\1/p" | head -1)
  fi
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

// extractKeycloakJSONIDByAttr mirrors the shell sed logic for unit tests.
func extractKeycloakJSONIDByAttr(json, attr, value string) string {
	before := regexp.MustCompile(`"id":"([^"]+)","` + regexp.QuoteMeta(attr) + `":"` + regexp.QuoteMeta(value))
	if m := before.FindStringSubmatch(json); len(m) > 1 {
		return m[1]
	}
	after := regexp.MustCompile(`"` + regexp.QuoteMeta(attr) + `":"` + regexp.QuoteMeta(value) + `","id":"([^"]+)"`)
	if m := after.FindStringSubmatch(json); len(m) > 1 {
		return m[1]
	}
	return ""
}
