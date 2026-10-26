/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

const testOIDCCatalogFixture = "../oidc/testdata/minimal-oidc-catalog.yaml"

func TestOIDCPacksNeedEntitlementGroups(t *testing.T) {
	c := oidc.NewTestClientWithCatalogFile(t, testOIDCCatalogFixture)
	pack, templates, ok, err := oidc.ResolvePack(context.Background(), c, "catalogue-test-app", "catalogue-test-client")
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
	pack, templates, ok, err := oidc.ResolvePack(context.Background(), c, "catalogue-test-app", "catalogue-test-client")
	if err != nil || !ok {
		t.Fatalf("resolve pack: ok=%v err=%v", ok, err)
	}
	script := buildOIDCPackScript("demo", "catalogue-test-client", pack, templates,
		[]string{"https://app.demo.platform.example.test/*"}, false, "gentian:tenant:demo:app:catalogue-test-client")
	for _, want := range []string{
		"catalogue-test-client-scope",
		"gentian:tenant:demo:app:catalogue-test-client",
		"PUBLIC_CLIENT=true",
		"https://app.demo.platform.example.test/*",
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
	// The Job no longer POSTs the scope's protocol mappers. app-default composes
	// a ProtocolMapper per entry in pack.Mappers, and those adopted the live
	// mappers by their Keycloak ids rather than creating new ones — verified on
	// corp, where all three kept their ids and their config.
	//
	// The mapper name is what identifies one, so a POST body naming a mapper is
	// the thing to assert is gone.
	for _, gone := range []string{
		`"name":"gentian_useruuid"`,
		`"name":"full name"`,
		`"protocolMapper":"oidc-usermodel-attribute-mapper"`,
		`"consentRequired":false`,
	} {
		if strings.Contains(script, gone) {
			t.Fatalf("oidc pack script still writes a protocol mapper: %s", gone)
		}
	}
	// The corrupt-mapper cleanup stays: it deletes mappers whose name is
	// literally the protocolMapper type, left by a much older failed run, and
	// nothing declarative covers that.
	if !strings.Contains(script, "removed corrupt mapper") {
		t.Fatal("the corrupt-mapper cleanup must stay")
	}
	// The client role is the Composition's, and the Composition is created
	// only once this Job has completed: a Job that needs the role waits for
	// itself. A client that exists without its role -- the reinstall after a
	// purge -- must complete here.
	for _, gone := range []string{"/roles/", "catalogue-test-client-access-control", "CLIENT_ROLE"} {
		if strings.Contains(script, gone) {
			t.Fatalf("oidc pack script still depends on the client role: %s", gone)
		}
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

func TestSubstituteTenantDomainInURIs(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Status.Domain = "demo.platform.example.test"
	uris := substituteTenantDomainInURIs(tenant,
		[]string{"https://app.${TENANT_DOMAIN}/*"}, "platform.example.test", gentianov1alpha1.TenancyModeMulti)
	if len(uris) != 1 || uris[0] != "https://app.demo.platform.example.test/*" {
		t.Fatalf("redirects: %v", uris)
	}
}

// fakeKeycloakCurl answers the pack script's calls the way Keycloak does for a
// realm where the client and its scope exist and the client role does not:
// the reinstall after a purge, before the App claim composes the role. Every
// call is logged to $CURL_LOG.
const fakeKeycloakCurl = `#!/bin/sh
out=""; method=GET; url=""; w=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -w) w=$2; shift ;;
    -X) method=$2; shift ;;
    -H|-d|--max-time) shift ;;
    http*) url=$1 ;;
  esac
  shift
done
echo "${method} ${url}" >> "${CURL_LOG}"
code=200
case "${url}" in
  */protocol/openid-connect/token) body='{"access_token":"t"}' ;;
  */client-scopes) body='[{"id":"scope-1","name":"catalogue-test-client-scope"}]' ;;
  */protocol-mappers/models) body='[]' ;;
  *"/clients?clientId="*) body='[{"id":"client-1","clientId":"catalogue-test-client"}]' ;;
  */roles/*) code=404; body='{"error":"Could not find role"}' ;;
  *) code=404; body='{}' ;;
esac
if [ -n "${out}" ]; then printf '%s' "${body}" > "${out}"; else printf '%s' "${body}"; fi
if [ -n "${w}" ]; then printf '%s' "${code}"; fi
if [ "${code}" -ge 400 ] && [ -z "${w}" ]; then exit 22; fi
exit 0
`

// The Job is waited on before the App claim exists, and the claim's
// Composition is what makes the client role. A client found without its role
// must therefore complete the Job: failing on it waited for itself, and the
// tenant never reached the app (nextcloud on test, after a purge).
func TestTheOIDCPackScriptCompletesWhenTheClientRoleIsNotThereYet(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed; the script resolves ids with it")
	}
	c := oidc.NewTestClientWithCatalogFile(t, testOIDCCatalogFixture)
	pack, templates, ok, err := oidc.ResolvePack(context.Background(), c, "catalogue-test-app", "catalogue-test-client")
	if err != nil || !ok {
		t.Fatalf("resolve pack: ok=%v err=%v", ok, err)
	}
	script := buildOIDCPackScript("demo", "catalogue-test-client", pack, templates,
		[]string{"https://app.demo.platform.example.test/*"}, false, "gentian:tenant:demo:app:catalogue-test-client")

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/curl", []byte(fakeKeycloakCurl), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/pack.sh", []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", dir+"/pack.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"CURL_LOG="+dir+"/calls",
		"KEYCLOAK_URL=http://keycloak.test",
		"KEYCLOAK_ADMIN_USERNAME=admin", "KEYCLOAK_ADMIN_PASSWORD=pw")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed where the client exists without its role: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "client catalogue-test-client already exists") ||
		!strings.Contains(string(out), "oidc pack catalogue-test-client provisioned") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	calls, _ := os.ReadFile(dir + "/calls")
	if strings.Contains(string(calls), "/roles/") {
		t.Fatalf("the script still asks for the client role:\n%s", calls)
	}
}
