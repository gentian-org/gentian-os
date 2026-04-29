package controller

import (
	"strings"
	"testing"
)

func TestBuildAdminScript_UsesSafeAuthHeaderExpansion(t *testing.T) {
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
}
