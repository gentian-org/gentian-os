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
	if !strings.Contains(script, `keycloak_json_id_by_attr ${SCOPE_LIST} "name" "${SCOPE_NAME}"`) {
		t.Fatal("expected SCOPE_UUID via keycloak_json_id_by_attr")
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
