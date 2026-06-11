// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"os"
	"os/exec"
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

func TestExtractKeycloakJSONIDByAttr_FieldsBetweenNameAndID(t *testing.T) {
	t.Parallel()
	// Keycloak ClientScopeRepresentation often places id after description/protocol.
	json := `[{"name":"opendesk-jitsi-scope","description":"Scope for openDesk","protocol":"openid-connect","id":"scope-uuid-1"}]`
	got := extractKeycloakJSONIDByAttr(json, "name", "opendesk-jitsi-scope")
	if got != "scope-uuid-1" {
		t.Fatalf("got id %q", got)
	}
}

func TestExtractKeycloakJSONIDByAttr_MultiObjectArray(t *testing.T) {
	t.Parallel()
	json := `[{"name":"email","id":"a"},{"name":"opendesk-jitsi-scope","description":"x","id":"b"}]`
	got := extractKeycloakJSONIDByAttr(json, "name", "opendesk-jitsi-scope")
	if got != "b" {
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
	if !strings.Contains(script, `keycloak_json_id_by_attr "${LDAP_COMPONENTS}" "name" "ldap"`) {
		t.Fatal("expected quoted LDAP_COMPONENTS JSON for keycloak_json_id_by_attr")
	}
	if !strings.Contains(script, `"${KEYCLOAK_URL}/admin/realms/demo")`) {
		t.Fatal("realm script must not corrupt HTTP realm URL with misplaced format args")
	}
	if strings.Contains(script, "keycloak_json_id_by_attr ${LDAP_COMPONENTS}") &&
		strings.Contains(script, "/admin/realms/keycloak_json") {
		t.Fatal("realm script must not splice ldapIDBlock into the HTTP curl line")
	}

	if strings.Contains(script, firstBrokerLoginFlowAlias) {
		t.Fatal("realm script must register kernel IdP with built-in first broker login flow only")
	}
	if !strings.Contains(script, `\"firstBrokerLoginFlowAlias\":\"first broker login\"`) {
		t.Fatal("realm script must use built-in first broker login flow for initial IdP registration")
	}

	path := t.TempDir() + "/realm.sh"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("realm script must be valid POSIX sh: %v\n%s", err, out)
	}
}
