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
    _kj_id=$(printf '%s' "${_kj_json}" | jq -r --arg a "${_kj_attr}" --arg v "${_kj_val}" '(if type == "array" then .[] elif (.content? | type) == "array" then .content[] else empty end) | select(.[$a] == $v) | .id // empty' 2>/dev/null | head -1)
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

// keycloakShellRequireID emits shell that assigns outVar from the extractor or exits 1.
func keycloakShellRequireID(outVar, jsonVar, attr, value string) string {
	return fmt.Sprintf(`keycloak_json_id_by_attr %s "%s" "%s"
%s="${_kj_id}"
if [ -z "${%s}" ]; then
  echo "ERROR: could not resolve Keycloak resource id (%s=%s)" >&2
  exit 1
fi`, jsonVar, attr, value, outVar, outVar, attr, value)
}

// extractKeycloakJSONIDByAttr mirrors the shell logic for unit tests (jq when available).
func extractKeycloakJSONIDByAttr(raw, attr, value string) string {
	var items []map[string]any
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		var wrapped struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapped); err == nil && len(wrapped.Content) > 0 {
			items = wrapped.Content
		}
	}
	if items == nil {
		var arr []map[string]any
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			items = arr
		}
	}
	for _, item := range items {
		if fmt.Sprint(item[attr]) == value {
			if id, ok := item["id"].(string); ok && id != "" {
				return id
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
