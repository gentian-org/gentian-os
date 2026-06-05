package controller

import (
	"strings"
	"testing"
)

func TestBuildAdminScript_UsesSafeAuthHeaderExpansion(t *testing.T) {
	t.Parallel()
	script := buildAdminScript("gtn-demo")

	if strings.Contains(script, "AUTH=\"-H") {
		t.Fatalf("script should not construct AUTH as embedded shell arguments")
	}
	if strings.Contains(script, "${AUTH}") {
		t.Fatalf("script should not expand ${AUTH} in curl calls")
	}
	if !strings.Contains(script, "AUTH_HEADER=\"Authorization: Bearer ${TOKEN}\"") {
		t.Fatalf("script must define AUTH_HEADER")
	}
	if !strings.Contains(script, "curl -sf -H \"${AUTH_HEADER}\"") {
		t.Fatalf("script must pass authorization via -H \"${AUTH_HEADER}\"")
	}
	if !strings.Contains(script, "INITIAL_TENANT_ADMIN realm=") {
		t.Fatal("script must emit INITIAL_TENANT_ADMIN after password sync")
	}
	if strings.Contains(script, "password reset skipped") {
		t.Fatal("script must always sync tenant admin password from OpenBao")
	}
	if !strings.Contains(script, "federationLink") {
		t.Fatal("script must skip Keycloak reset-password for LDAP-federated users")
	}
}
