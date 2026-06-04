// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

func TestBuildOIDCPackScriptJitsi(t *testing.T) {
	packs, templates, err := oidc.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	pack := packs["opendesk-jitsi"]
	script := buildOIDCPackScript("demo", "opendesk-jitsi", pack, templates,
		[]string{"https://meet.demo.desk.gentian.org/*"}, "")
	for _, want := range []string{
		"opendesk-jitsi-scope",
		"opendesk-jitsi-access-control",
		"managed-by-attribute-Videoconference",
		"PUBLIC_CLIENT=true",
		"https://meet.demo.desk.gentian.org/*",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
	if strings.Contains(script, `tr ',' '\n' | grep -F "\"name\":\"${SCOPE_NAME}\""`) {
		t.Fatal("oidc pack script must not use fragile tr/grep scope id extraction")
	}
	if !strings.Contains(script, "_kj_scope_id_from_list") {
		t.Fatal("expected dedicated client-scope id lookup helper")
	}
	if strings.Contains(script, `\"name\":"opendesk_useruuid"`) {
		t.Fatal("mapper POST JSON must quote name field values")
	}
	if !strings.Contains(script, `"name":"opendesk_useruuid"`) || !strings.Contains(script, `"protocolMapper":"oidc-usermodel-attribute-mapper"`) {
		t.Fatal("mapper POST body must include opendesk_useruuid name and usermodel protocolMapper")
	}
	if !strings.Contains(script, `"consentRequired":false`) {
		t.Fatal("mapper POST body must set consentRequired false")
	}
	if !strings.Contains(script, `"multivalued":"false"`) {
		t.Fatal("usermodel mappers must include multivalued false")
	}
	if !strings.Contains(script, `"name":"full name"`) {
		t.Fatal("full_name template must map to Keycloak mapper name \"full name\"")
	}
	if !strings.Contains(script, `keycloak_json_id_by_attr "${EXISTING}" "clientId"`) {
		t.Fatal("client UUID lookup must quote EXISTING JSON")
	}
	if !strings.Contains(script, "default-default-client-scopes") {
		t.Fatal("expected fallback lookup on default-default-client-scopes")
	}
	path := t.TempDir() + "/oidc-pack.sh"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("oidc pack script must be valid POSIX sh: %v\n%s", err, out)
	}
}

func TestBuildOIDCBrowserFlowScript(t *testing.T) {
	script := buildOIDCBrowserFlowScript("demo")
	if !strings.Contains(script, "browser-kernel-idp") {
		t.Fatal("expected browser-kernel-idp flow alias")
	}
	if !strings.Contains(script, "defaultProvider") {
		t.Fatal("expected IdP redirector defaultProvider config")
	}
}

func TestResolveOIDCRedirectURIsFromProfile(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Spec.Domain = "demo.desk.gentian.org"
	uris := resolveOIDCRedirectURIs(tenant, "jitsi",
		[]string{"https://meet.${TENANT_DOMAIN}/*"}, "desk.gentian.org")
	if len(uris) != 1 || uris[0] != "https://meet.demo.desk.gentian.org/*" {
		t.Fatalf("redirects: %v", uris)
	}
}
