/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package keycloak

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestExtractJSONIDByAttr_UserStorageProvider(t *testing.T) {
	t.Parallel()
	json := `[{"id":"f47ac10b-58cc-4372-a567-0e02b2c3d479","name":"federation","providerId":"custom-user-storage","providerType":"org.keycloak.storage.UserStorageProvider","config":{"connectionUrl":["https://idp.example/storage"]}}]`
	got := extractJSONIDByAttr(json, "name", "federation")
	if got != "f47ac10b-58cc-4372-a567-0e02b2c3d479" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractJSONIDByAttr_IDAfterAttribute(t *testing.T) {
	t.Parallel()
	json := `[{"name":"federation","id":"abc-def-123","providerId":"custom-user-storage"}]`
	got := extractJSONIDByAttr(json, "name", "federation")
	if got != "abc-def-123" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractJSONIDByAttr_FieldsBetweenNameAndID(t *testing.T) {
	t.Parallel()
	json := `[{"name":"catalogue-test-client-scope","description":"Scope for tests","protocol":"openid-connect","id":"scope-uuid-1"}]`
	got := extractJSONIDByAttr(json, "name", "catalogue-test-client-scope")
	if got != "scope-uuid-1" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractJSONIDByAttr_MultiObjectArray(t *testing.T) {
	t.Parallel()
	json := `[{"name":"email","id":"a"},{"name":"catalogue-test-client-scope","description":"x","id":"b"}]`
	got := extractJSONIDByAttr(json, "name", "catalogue-test-client-scope")
	if got != "b" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractJSONIDByAttr_BrokerClient(t *testing.T) {
	t.Parallel()
	json := `[{"id":"client-uuid","clientId":"broker-demo","protocol":"openid-connect"}]`
	got := extractJSONIDByAttr(json, "clientId", "broker-demo")
	if got != "client-uuid" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractJSONIDByAttr_NotFound(t *testing.T) {
	t.Parallel()
	if got := extractJSONIDByAttr(`[{"id":"x","name":"other"}]`, "name", "federation"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// extractJSONIDByAttr mirrors the shell logic (jq when available).
func extractJSONIDByAttr(raw, attr, value string) string {
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
