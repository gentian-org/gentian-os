package controller

import (
	"strings"
	"testing"
)

func TestTenantSMTPJobName(t *testing.T) {
	got := tenantSMTPJobName("demo")
	want := "keycloak-tenant-smtp-demo"
	if got != want {
		t.Fatalf("tenantSMTPJobName = %q, want %q", got, want)
	}
}

func TestBuildTenantSMTPConfigureScriptRealmPlaceholderCount(t *testing.T) {
	script := buildTenantSMTPConfigureScript(`"demo"`)
	for _, want := range []string{`REALM="demo"`, "SMTP_HOST", "smtpServer = $smtp"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
}
