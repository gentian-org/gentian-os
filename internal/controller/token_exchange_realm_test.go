/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gentian-org/gentian-os/internal/bouncer"
)

// What a realm has to hold for a session's token to be exchanged for an
// app's, read off what the tenant Composition renders. Three fixtures, which
// are the three kinds of realm there are: a tenant's own, the kernel realm a
// tenant adopts, and the user tenant's of a single-tenancy cluster.
var exchangeRealmFixtures = []struct {
	fixture, tenant, realm, zone string
}{
	{"tenant-default", "render-fixture", "render-fixture", "render-fixture"},
	{"tenant-platform", "platform", "kernel", "kernel"},
	{"tenant-single-user", "user", "user", "user"},
}

// exchangeClientOf is the one Client a render makes under the exchange
// client's id.
func exchangeClientOf(t *testing.T, fixture string, objs []map[string]interface{}) map[string]interface{} {
	t.Helper()
	var found []map[string]interface{}
	for _, o := range ofKind(objs, "Client") {
		if id, _, _ := unstructured.NestedString(o, "spec", "forProvider", "clientId"); id == bouncer.ExchangeClientID {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d clients called %s, want exactly one", fixture, len(found), bouncer.ExchangeClientID)
	}
	return found[0]
}

// audienceMappersNaming are the mappers that put one name in an audience,
// whichever of the two ways they say it.
func audienceMappersNaming(objs []map[string]interface{}, audience string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, o := range ofKind(objs, "ProtocolMapper") {
		cfg, _, _ := unstructured.NestedStringMap(o, "spec", "forProvider", "config")
		for k, v := range cfg {
			if strings.HasPrefix(k, "included.") && v == audience {
				out = append(out, o)
			}
		}
	}
	return out
}

func customAudience(audience string) map[string]string {
	return map[string]string{
		// Custom, not client: neither the exchange client nor an app has to
		// exist as a client of the realm for the name to reach the token,
		// and Keycloak drops a client audience whose client does not.
		"included.custom.audience":  audience,
		"access.token.claim":        "true",
		"id.token.claim":            "false",
		"userinfo.token.claim":      "false",
		"introspection.token.claim": "true",
	}
}

// The exchange client can exchange and can do nothing else: nobody signs in
// through it, it is nobody itself, and the token it is handed back carries
// no more than the scope that was asked for.
func TestTheExchangeClientAsRendered(t *testing.T) {
	for _, c := range exchangeRealmFixtures {
		objs := renderedObjects(t, c.fixture)
		client := exchangeClientOf(t, c.fixture, objs)

		if realm, _, _ := unstructured.NestedString(client, "spec", "forProvider", "realmId"); realm != c.realm {
			t.Errorf("%s: the exchange client is in realm %q, want %q", c.fixture, realm, c.realm)
		}
		if access, _, _ := unstructured.NestedString(client, "spec", "forProvider", "accessType"); access != "CONFIDENTIAL" {
			t.Errorf("%s: accessType %q: a client without a secret could be anybody", c.fixture, access)
		}
		for _, flow := range []string{"standardFlowEnabled", "implicitFlowEnabled", "directAccessGrantsEnabled", "serviceAccountsEnabled"} {
			on, has, _ := unstructured.NestedBool(client, "spec", "forProvider", flow)
			if !has || on {
				t.Errorf("%s: %s is not stated false: the exchange client has a way to a token besides the exchange", c.fixture, flow)
			}
		}
		// Stated, because Keycloak's default is true.
		if full, has, _ := unstructured.NestedBool(client, "spec", "forProvider", "fullScopeAllowed"); !has || full {
			t.Errorf("%s: fullScopeAllowed is not stated false: an exchanged token would carry every role of the person", c.fixture)
		}
		if on, _, _ := unstructured.NestedBool(client, "spec", "forProvider", "standardTokenExchangeEnabled"); !on {
			t.Errorf("%s: standard token exchange is not switched on, and it is the one thing this client is for", c.fixture)
		}
		for _, f := range []string{"validRedirectUris", "rootUrl", "baseUrl"} {
			if _, has, _ := unstructured.NestedFieldNoCopy(client, "spec", "forProvider", f); has {
				t.Errorf("%s: the exchange client states %s, and nothing is ever redirected to it", c.fixture, f)
			}
		}
		// Not LateInitialize, as on the zone's client: the declared intent
		// would become whatever the live client holds.
		policies, _, _ := unstructured.NestedStringSlice(client, "spec", "managementPolicies")
		if !reflect.DeepEqual(policies, []string{"Observe", "Create", "Update", "Delete"}) {
			t.Errorf("%s: managementPolicies %v", c.fixture, policies)
		}

		// One default scope where the provider cannot hold none: see the
		// Composition. Anything more is in every exchanged token.
		clientName, _, _ := unstructured.NestedString(client, "metadata", "name")
		var defaults [][]string
		for _, o := range ofKind(objs, "ClientDefaultScopes") {
			if ref, _, _ := unstructured.NestedString(o, "spec", "forProvider", "clientIdRef", "name"); ref == clientName {
				scopes, _, _ := unstructured.NestedStringSlice(o, "spec", "forProvider", "defaultScopes")
				defaults = append(defaults, scopes)
			}
		}
		if len(defaults) != 1 || !reflect.DeepEqual(defaults[0], []string{"basic"}) {
			t.Errorf("%s: the exchange client's default scopes are %v, want one list holding basic alone", c.fixture, defaults)
		}
	}
}

// Keycloak admits a client to an exchange only for a token that names it as
// an audience. The session's token is the zone client's, so the mapper is
// there -- and only there, since a mapper on anything else would let the
// tokens of some other client be exchanged.
func TestTheZoneTokenNamesTheExchangeClient(t *testing.T) {
	for _, c := range exchangeRealmFixtures {
		objs := renderedObjects(t, c.fixture)
		mappers := audienceMappersNaming(objs, bouncer.ExchangeClientID)
		if len(mappers) != 1 {
			t.Errorf("%s: %d mappers put %s in an audience, want exactly the one on the zone's client", c.fixture, len(mappers), bouncer.ExchangeClientID)
			continue
		}
		mapper := mappers[0]
		if ref, _, _ := unstructured.NestedString(mapper, "spec", "forProvider", "clientIdRef", "name"); ref != c.tenant+"-edge-zone-client" {
			t.Errorf("%s: the mapper is on %q, not on the zone's client", c.fixture, ref)
		}
		if realm, _, _ := unstructured.NestedString(mapper, "spec", "forProvider", "realmId"); realm != c.realm {
			t.Errorf("%s: the mapper is in realm %q, want %q", c.fixture, realm, c.realm)
		}
		if pm, _, _ := unstructured.NestedString(mapper, "spec", "forProvider", "protocolMapper"); pm != "oidc-audience-mapper" {
			t.Errorf("%s: the mapper is a %q", c.fixture, pm)
		}
		cfg, _, _ := unstructured.NestedStringMap(mapper, "spec", "forProvider", "config")
		if want := customAudience(bouncer.ExchangeClientID); !reflect.DeepEqual(cfg, want) {
			t.Errorf("%s: the mapper's config is\n  %v\nwant\n  %v", c.fixture, cfg, want)
		}
	}
}

// One scope per profile that asks, called what the route builder calls it,
// and on it the one mapper that names the app in an access token's audience.
// An app that does not ask has no scope, though it is installed.
func TestOneAppScopePerProfileThatAsks(t *testing.T) {
	appScopes := func(objs []map[string]interface{}) map[string]map[string]interface{} {
		out := map[string]map[string]interface{}{}
		for _, o := range ofKind(objs, "ClientScope") {
			if name, _, _ := unstructured.NestedString(o, "spec", "forProvider", "name"); strings.HasPrefix(name, AppTokenScope("")) {
				out[name] = o
			}
		}
		return out
	}
	optionalScopes := func(fixture string, objs []map[string]interface{}) ([]string, bool) {
		clientName, _, _ := unstructured.NestedString(exchangeClientOf(t, fixture, objs), "metadata", "name")
		for _, o := range ofKind(objs, "ClientOptionalScopes") {
			if ref, _, _ := unstructured.NestedString(o, "spec", "forProvider", "clientIdRef", "name"); ref == clientName {
				scopes, _, _ := unstructured.NestedStringSlice(o, "spec", "forProvider", "optionalScopes")
				return scopes, true
			}
		}
		return nil, false
	}

	// notes and wiki ask (the fixture's projection); catalogue-test-app is
	// on spec.apps and does not.
	objs := renderedObjects(t, "tenant-default")
	scopes := appScopes(objs)
	asking := []string{"notes", "wiki"}
	if len(scopes) != len(asking) {
		t.Fatalf("tenant-default renders %d app scopes, want one for each of %v", len(scopes), asking)
	}
	for _, profile := range asking {
		// AppTokenScope is what writes the name onto the app's routes for
		// the bouncer to ask for. The realm has to hold it under that name.
		scope, ok := scopes[AppTokenScope(profile)]
		if !ok {
			t.Fatalf("no client scope %q: the bouncer would ask the realm for a scope it does not hold", AppTokenScope(profile))
		}
		if in, _, _ := unstructured.NestedBool(scope, "spec", "forProvider", "includeInTokenScope"); !in {
			t.Errorf("%s is not included in the token's scope claim", AppTokenScope(profile))
		}
		if realm, _, _ := unstructured.NestedString(scope, "spec", "forProvider", "realmId"); realm != "render-fixture" {
			t.Errorf("%s is in realm %q", AppTokenScope(profile), realm)
		}

		mappers := audienceMappersNaming(objs, profile)
		if len(mappers) != 1 {
			t.Fatalf("%d mappers put %s in an audience, want exactly the one on its scope", len(mappers), profile)
		}
		mapper := mappers[0]
		cfg, _, _ := unstructured.NestedStringMap(mapper, "spec", "forProvider", "config")
		if want := customAudience(profile); !reflect.DeepEqual(cfg, want) {
			t.Errorf("%s: the audience mapper's config is\n  %v\nwant\n  %v", profile, cfg, want)
		}
		if pm, _, _ := unstructured.NestedString(mapper, "spec", "forProvider", "protocolMapper"); pm != "oidc-audience-mapper" {
			t.Errorf("%s: the mapper is a %q", profile, pm)
		}
		// On the scope, not on a client: a mapper on the exchange client
		// would put this app in every token it is handed, whichever app the
		// exchange was for.
		if id, _, _ := unstructured.NestedString(mapper, "spec", "forProvider", "clientScopeId"); id == "" {
			t.Errorf("%s: the audience mapper is not on a client scope", profile)
		}
		for _, f := range []string{"clientId", "clientIdRef", "clientIdSelector"} {
			if _, has, _ := unstructured.NestedFieldNoCopy(mapper, "spec", "forProvider", f); has {
				t.Errorf("%s: the audience mapper is on a client (%s)", profile, f)
			}
		}
	}
	if _, has := scopes[AppTokenScope("catalogue-test-app")]; has {
		t.Error("an installed app that does not ask for an exchanged token was given a scope")
	}
	if len(audienceMappersNaming(objs, "catalogue-test-app")) != 0 {
		t.Error("an installed app that does not ask is named in an audience")
	}
	// The exchange client's optional scopes are the app scopes and nothing
	// else -- none of the five Keycloak gives every client.
	got, has := optionalScopes("tenant-default", objs)
	if want := []string{AppTokenScope("notes"), AppTokenScope("wiki")}; !has || !reflect.DeepEqual(got, want) {
		t.Errorf("the exchange client's optional scopes are %v, want %v", got, want)
	}

	// The first pass after an app starts asking: the scope, and nothing that
	// needs the scope to exist already.
	objs = renderedObjects(t, "tenant-single-user")
	scopes = appScopes(objs)
	if _, ok := scopes[AppTokenScope("cloud")]; !ok || len(scopes) != 1 {
		t.Fatalf("tenant-single-user renders %d app scopes, want the one for cloud", len(scopes))
	}
	if len(audienceMappersNaming(objs, "cloud")) != 0 {
		t.Error("a mapper is rendered on a scope whose id has not been observed")
	}
	if got, has := optionalScopes("tenant-single-user", objs); has {
		t.Errorf("optional scopes %v are rendered before any of them exists", got)
	}

	// No app asks: no scope, and no list -- the provider cannot hold an
	// empty one, so it is not rendered rather than rendered empty.
	objs = renderedObjects(t, "tenant-platform")
	if scopes = appScopes(objs); len(scopes) != 0 {
		t.Errorf("tenant-platform renders %d app scopes with no app asking", len(scopes))
	}
	if got, has := optionalScopes("tenant-platform", objs); has {
		t.Errorf("tenant-platform renders optional scopes %v with no app asking", got)
	}
}

// The exchange client's secret is where the operator looks for it and says
// what the operator reads from it: a Secret of the edge namespace, labelled
// with the realm it belongs to, holding the secret under one key. The realm
// and not the tenant -- for the tenant that adopts the kernel realm the two
// differ, and the realm is what a token is exchanged at.
func TestTheExchangeSecretIsFoundByItsRealm(t *testing.T) {
	const edgeNamespace = "kernel-edge"
	for _, c := range exchangeRealmFixtures {
		objs := renderedObjects(t, c.fixture)
		secretName := "edge-" + c.zone + "-exchange"

		manifest := func(kind string) map[string]interface{} {
			t.Helper()
			for _, o := range ofKind(objs, "Object") {
				m, _, _ := unstructured.NestedMap(o, "spec", "forProvider", "manifest")
				name, _, _ := unstructured.NestedString(m, "metadata", "name")
				if m["kind"] == kind && name == secretName {
					return m
				}
			}
			t.Fatalf("%s: no %s named %s", c.fixture, kind, secretName)
			return nil
		}

		es := manifest("ExternalSecret")
		if ns, _, _ := unstructured.NestedString(es, "metadata", "namespace"); ns != edgeNamespace {
			t.Errorf("%s: the Secret is written to %q, and the operator gathers them from %q", c.fixture, ns, edgeNamespace)
		}
		if target, _, _ := unstructured.NestedString(es, "spec", "target", "name"); target != secretName {
			t.Errorf("%s: the Secret is called %q, want %q", c.fixture, target, secretName)
		}
		// On the Secret itself, which is what the operator lists; a label on
		// the ExternalSecret alone reaches nothing.
		labels, _, _ := unstructured.NestedStringMap(es, "spec", "target", "template", "metadata", "labels")
		if labels[exchangeRealmLabel] != c.realm {
			t.Errorf("%s: the Secret's %s is %q, want the realm %q", c.fixture, exchangeRealmLabel, labels[exchangeRealmLabel], c.realm)
		}
		if !exchangeRealmName.MatchString(labels[exchangeRealmLabel]) {
			t.Errorf("%s: the operator would skip a Secret of realm %q", c.fixture, labels[exchangeRealmLabel])
		}
		// Generated once: a second answer from the generator is a second
		// secret, at an hour nobody chose.
		if p, _, _ := unstructured.NestedString(es, "spec", "refreshPolicy"); p != "CreatedOnce" {
			t.Errorf("%s: refreshPolicy %q", c.fixture, p)
		}
		if p, _, _ := unstructured.NestedString(es, "spec", "target", "creationPolicy"); p != "Owner" {
			t.Errorf("%s: creationPolicy %q", c.fixture, p)
		}

		// The key is the generator's to choose, and the operator and the
		// Client both read it by name.
		keys, _, _ := unstructured.NestedStringSlice(manifest("Password"), "spec", "secretKeys")
		if !reflect.DeepEqual(keys, []string{exchangeSecretKey}) {
			t.Errorf("%s: the generator writes %v, and the operator reads %q", c.fixture, keys, exchangeSecretKey)
		}
		ref, _, _ := unstructured.NestedStringMap(exchangeClientOf(t, c.fixture, objs), "spec", "forProvider", "clientSecretSecretRef")
		want := map[string]string{"name": secretName, "namespace": edgeNamespace, "key": exchangeSecretKey}
		if !reflect.DeepEqual(ref, want) {
			t.Errorf("%s: the client is pushed %v, want the generated Secret %v", c.fixture, ref, want)
		}
	}
}
