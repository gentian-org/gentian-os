/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/


package controller

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

const testOIDCCatalogFixture = "../oidc/testdata/minimal-oidc-catalog.yaml"

func TestOIDCPacksNeedEntitlementGroups(t *testing.T) {
	c := oidc.NewTestClientWithCatalogFile(t, testOIDCCatalogFixture)
	pack, templates, ok, err := oidc.ResolvePack(context.Background(), c, "catalogue-test-client")
	if err != nil || !ok {
		t.Fatalf("resolve pack: ok=%v err=%v", ok, err)
	}
	if !oidcPacksNeedEntitlementGroups([]oidcAppConfig{{pack: &pack, templates: templates}}) {
		t.Fatal("expected catalogue-test-client pack to require entitlement groups")
	}
	if oidcPacksNeedEntitlementGroups([]oidcAppConfig{{profileName: "custom-app"}}) {
		t.Fatal("expected custom client without pack to skip entitlement group gate")
	}
}

func TestBuildOIDCPackScript(t *testing.T) {
	c := oidc.NewTestClientWithCatalogFile(t, testOIDCCatalogFixture)
	pack, templates, ok, err := oidc.ResolvePack(context.Background(), c, "catalogue-test-client")
	if err != nil || !ok {
		t.Fatalf("resolve pack: ok=%v err=%v", ok, err)
	}
	script := buildOIDCPackScript("demo", "catalogue-test-client", pack, templates,
		[]string{"https://app.demo.desk.gentian.org/*"}, "", "gentian:tenant:demo:app:catalogue-test-client")
	for _, want := range []string{
		"catalogue-test-client-scope",
		"catalogue-test-client-access-control",
		"gentian:tenant:demo:app:catalogue-test-client",
		"PUBLIC_CLIENT=true",
		"https://app.demo.desk.gentian.org/*",
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
	if strings.Contains(script, `\"name\":"gentian_useruuid"`) {
		t.Fatal("mapper POST JSON must quote name field values")
	}
	if !strings.Contains(script, `"name":"gentian_useruuid"`) || !strings.Contains(script, `"protocolMapper":"oidc-usermodel-attribute-mapper"`) {
		t.Fatal("mapper POST body must include gentian_useruuid name and usermodel protocolMapper")
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

func TestBuildFirstBrokerLoginFlowScript(t *testing.T) {
	script := buildFirstBrokerLoginFlowScript("demo")
	if path := os.Getenv("DUMP_FIRST_BROKER_LOGIN_SCRIPT"); path != "" {
		if err := os.WriteFile(path, []byte(keycloak.ProvisionerBootstrap+script), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{
		firstBrokerLoginFlowAlias,
		`idp-detect-existing-broker-user`,
		`first broker login flow ${FLOW_ALIAS} ready`,
		`idp-auto-link`,
		`requirement\":\"REQUIRED`,
		`federated-identity/kernel`,
		`kernel broker link purge finished`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("first broker login script missing %q", want)
		}
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
	if !strings.Contains(script, "requirement") || !strings.Contains(script, "REQUIRED") {
		t.Fatal("expected IdP redirector execution reconciled to REQUIRED")
	}
	if !strings.Contains(script, "set to REQUIRED (defaultProvider=kernel)") {
		t.Fatal("expected IdP redirector REQUIRED reconciliation log line")
	}
}

func TestSubstituteTenantDomainInURIs(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Spec.Domain = "demo.desk.gentian.org"
	uris := substituteTenantDomainInURIs(tenant,
		[]string{"https://app.${TENANT_DOMAIN}/*"}, "desk.gentian.org", gentianov1alpha1.TenancyModeMulti)
	if len(uris) != 1 || uris[0] != "https://app.demo.desk.gentian.org/*" {
		t.Fatalf("redirects: %v", uris)
	}
}
