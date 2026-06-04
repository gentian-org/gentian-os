// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestExtractKeycloakJSONIDByAttr_LDAPProvider(t *testing.T) {
	t.Parallel()
	json := `[{"id":"f47ac10b-58cc-4372-a567-0e02b2c3d479","name":"ldap","providerId":"ldap","providerType":"org.keycloak.storage.UserStorageProvider","config":{"connectionUrl":["ldap://ldap.example:389"]}}]`
	got := extractKeycloakJSONIDByAttr(json, "name", "ldap")
	if got != "f47ac10b-58cc-4372-a567-0e02b2c3d479" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractKeycloakJSONIDByAttr_IDAfterAttribute(t *testing.T) {
	t.Parallel()
	json := `[{"name":"ldap","id":"abc-def-123","providerId":"ldap"}]`
	got := extractKeycloakJSONIDByAttr(json, "name", "ldap")
	if got != "abc-def-123" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractKeycloakJSONIDByAttr_BrokerClient(t *testing.T) {
	t.Parallel()
	json := `[{"id":"client-uuid","clientId":"broker-demo","protocol":"openid-connect"}]`
	got := extractKeycloakJSONIDByAttr(json, "clientId", "broker-demo")
	if got != "client-uuid" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractKeycloakJSONIDByAttr_NotFound(t *testing.T) {
	t.Parallel()
	if got := extractKeycloakJSONIDByAttr(`[{"id":"x","name":"other"}]`, "name", "ldap"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestBuildRealmScript_UsesKeycloakJSONIDExtractor(t *testing.T) {
	t.Parallel()
	script := buildRealmScript("demo", "Demo")
	if !strings.Contains(script, "keycloak_json_id_by_attr") {
		t.Fatal("expected keycloak_json_id_by_attr helper in realm script")
	}
	if strings.Contains(script, `tr ',' '\\n' | grep -F '"name":"ldap"'`) {
		t.Fatal("realm script must not use fragile tr/grep LDAP_ID extraction")
	}
	if !strings.Contains(script, `keycloak_json_id_by_attr ${LDAP_COMPONENTS} "name" "ldap"`) {
		t.Fatal("expected LDAP_ID resolution via keycloak_json_id_by_attr")
	}
}
