/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// No fixture observes the pack's client scope, so none renders its mappers.
func TestPackScopeMappersRenderOnceTheScopeIsObserved(t *testing.T) {
	in := &Input{
		App:         "demo-app",
		Namespace:   "tenant-demo",
		TenantName:  "demo",
		ProfileName: "demo-app",
		Profile: &gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "demo"}},
			}},
		},
		Packs: &gentianov1alpha1.OIDCPackCatalogSpec{
			MapperTemplates: map[string]gentianov1alpha1.OIDCMapperTemplate{
				"full_name": {
					KeycloakName:   "full name",
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config:         map[string]string{"claim.name": "name"},
				},
				"groups": {
					ProtocolMapper: "oidc-group-membership-mapper",
					Config:         map[string]string{"introspection.token.claim": "false"},
				},
				"unknown": {},
			},
			Packs: map[string]gentianov1alpha1.OIDCPackSpec{
				"demo": {ScopeName: "demo-scope", Mappers: []string{"full_name", "groups", "unknown", "missing"}},
			},
		},
		Observed: map[string]*unstructured.Unstructured{},
	}
	mappers := func() map[string]*unstructured.Unstructured {
		out := &Rendered{}
		if err := renderIdentity(in, out); err != nil {
			t.Fatal(err)
		}
		m := map[string]*unstructured.Unstructured{}
		for _, obj := range out.Objects {
			if obj.GetKind() == "ProtocolMapper" {
				m[obj.GetName()] = obj
			}
		}
		return m
	}
	if got := mappers(); len(got) != 0 {
		t.Fatalf("mappers rendered before the scope's id is known: %v", got)
	}

	in.Observed["demo-demo-app-keycloak-client-scope"] = &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"atProvider": map[string]interface{}{"id": "1234-uuid"}},
	}}
	got := mappers()
	if len(got) != 2 {
		t.Fatalf("want the two mappers with a template, got %d", len(got))
	}
	full := got["demo-demo-app-scope-mapper-full-name"]
	if full == nil {
		t.Fatal("full_name mapper not rendered under its slug")
	}
	fp, _, _ := unstructured.NestedMap(full.Object, "spec", "forProvider")
	want := map[string]interface{}{
		"realmId":        "demo",
		"name":           "full name",
		"protocol":       "openid-connect",
		"protocolMapper": "oidc-usermodel-attribute-mapper",
		"clientScopeId":  "1234-uuid",
		"config": map[string]interface{}{
			"claim.name":                "name",
			"multivalued":               "false",
			"introspection.token.claim": "true",
		},
	}
	if diff := cmp.Diff(want, fp); diff != "" {
		t.Errorf("full_name mapper (-want +got):\n%s", diff)
	}
	cfg, _, _ := unstructured.NestedStringMap(got["demo-demo-app-scope-mapper-groups"].Object, "spec", "forProvider", "config")
	if diff := cmp.Diff(map[string]string{"introspection.token.claim": "false"}, cfg); diff != "" {
		t.Errorf("groups mapper keeps its own introspection value, no multivalued (-want +got):\n%s", diff)
	}
}

// spec.sessionMaxAge is the realm's cap on every OIDC client the profile's
// app signs in with: its own and an extension's, which runs in its pod. A
// profile that does not set it leaves the clients on the realm's limits.
func TestSessionMaxAgeCapsEveryClientOfTheApp(t *testing.T) {
	clients := func(in *Input) map[string]string {
		t.Helper()
		got, err := Render(in)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		out := map[string]string{}
		for _, o := range got.Objects {
			if o.GetKind() != "Client" || o.GroupVersionKind().Group != "openidclient.keycloak.crossplane.io" {
				continue
			}
			v, _, _ := unstructured.NestedString(o.Object, "spec", "forProvider", "clientSessionMaxLifespan")
			out[o.GetName()] = v
		}
		return out
	}

	unset := clients(fixtureInput(t, "testdata/render/app-sidecar-oidc"))
	if len(unset) != 2 {
		t.Fatalf("want the app's client and its extension's, got %v", unset)
	}
	for name, v := range unset {
		if v != "" {
			t.Errorf("%s: capped at %q with no sessionMaxAge", name, v)
		}
	}

	in := fixtureInput(t, "testdata/render/app-sidecar-oidc")
	in.Profile.SessionMaxAge = &metav1.Duration{Duration: 90 * time.Minute}
	capped := clients(in)
	if len(capped) != 2 {
		t.Fatalf("want the app's client and its extension's, got %v", capped)
	}
	for name, v := range capped {
		if v != "1h30m0s" {
			t.Errorf("%s: clientSessionMaxLifespan = %q, want 1h30m0s", name, v)
		}
	}
}

// The client of a token entry exists while the operator says the entry is
// served, and is one a program on a person's computer can use and nothing
// more: public with PKCE, consent, no other grant, no role in its tokens, no
// token that outlives the session, and tokens made out to the entry.
func TestATokenEntrysClientIsRenderedOnlyWhileTheEntryIsServed(t *testing.T) {
	in := &Input{
		App:         "agent-door",
		Namespace:   "tenant-demo",
		TenantName:  "demo",
		ProfileName: "agent-door",
		Profile: &gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "agent-door", AccessType: "CONFIDENTIAL"}},
			}},
		},
		Observed: map[string]*unstructured.Unstructured{},
	}
	render := func() map[string]*unstructured.Unstructured {
		out := &Rendered{}
		if err := renderIdentity(in, out); err != nil {
			t.Fatal(err)
		}
		m := map[string]*unstructured.Unstructured{}
		for _, o := range out.Objects {
			m[o.GetName()] = o
		}
		return m
	}
	const client = "demo-agent-door-token-entry-client"
	if _, has := render()[client]; has {
		t.Fatal("a client is rendered for an entry that is not served")
	}
	// Served, with nowhere to send a program back to: nothing to register.
	in.TokenEntry = &TokenEntry{Host: "mcp.demo.example.org"}
	if _, has := render()[client]; has {
		t.Fatal("a client with no redirect address is rendered")
	}
	in.TokenEntry.RedirectURIs = []string{"http://localhost/callback"}
	objs := render()
	fp, _, _ := unstructured.NestedMap(objs[client].Object, "spec", "forProvider")
	want := map[string]interface{}{
		"clientId":                  "token-entry-agent-door",
		"name":                      "Programs connecting to mcp.demo.example.org",
		"realmId":                   "demo",
		"extraConfig":               map[string]interface{}{"gentianos.io/managed-by": "platform"},
		"accessType":                "PUBLIC",
		"pkceCodeChallengeMethod":   "S256",
		"standardFlowEnabled":       true,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"serviceAccountsEnabled":    false,
		"fullScopeAllowed":          false,
		"consentRequired":           true,
		"displayOnConsentScreen":    true,
		"validRedirectUris":         []interface{}{"http://localhost/callback"},
	}
	if diff := cmp.Diff(want, fp); diff != "" {
		t.Fatalf("client (-want +got):\n%s", diff)
	}
	for name, aud := range map[string]string{
		"demo-agent-door-token-entry-audience":     "https://mcp.demo.example.org",
		"demo-agent-door-token-entry-audience-own": "agent-door",
	} {
		got, _, _ := unstructured.NestedString(objs[name].Object, "spec", "forProvider", "config", "included.custom.audience")
		if got != aud {
			t.Errorf("%s names %q, want %q", name, got, aud)
		}
	}
	scopes, _, _ := unstructured.NestedStringSlice(objs["demo-agent-door-token-entry-optional-scopes"].Object, "spec", "forProvider", "optionalScopes")
	for _, s := range scopes {
		if s == "offline_access" {
			t.Fatal("the client may ask for a token that outlives the session")
		}
	}
	if len(scopes) == 0 {
		t.Fatal("no optional scopes are declared, which leaves the realm's, offline_access among them")
	}
}
