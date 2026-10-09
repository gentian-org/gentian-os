/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// What a realm has to hold for a component to vouch for a person
// (requires.services.vouching), read off what the tenant Composition renders.
// The names are the operator's -- VouchingAlias, VouchingIssuer,
// VouchingSecretName and the Secret's keys, in vouching.go -- and every
// assertion below asks those functions rather than repeating what they
// return: the registrar links people to the alias and the component reads the
// Secret, so a name made differently here is a grant that never succeeds.
type vouchingComponent struct {
	profile string
	// jwks is where the realm fetches the component's keys, put together by
	// the Composition from the projection's line and the tenant's namespace.
	jwks string
}

var vouchingRealmFixtures = []struct {
	fixture, tenant, realm, namespace string
	// internalURL is Keycloak as reached from inside the cluster, from the
	// fixture's cluster config.
	internalURL string
	components  []vouchingComponent
	// delivered says the fixture has observed the generated secrets, so the
	// sequencer has let the Secret and the client through.
	delivered bool
}{
	{
		fixture: "tenant-default", tenant: "render-fixture", realm: "render-fixture", namespace: "tenant-render-fixture",
		internalURL: "http://gentian-idp-keycloak-keycloakx-http.kernel-authentication.svc.cluster.local:8080/auth",
		components: []vouchingComponent{
			// The default path, on a Service called what the profile is.
			{"scribe", "http://scribe.tenant-render-fixture.svc.cluster.local:8080/.well-known/jwks.json"},
			// Another Service name, port and path: none of the four fields of
			// a line is taken from another.
			{"steward", "http://steward-keys.tenant-render-fixture.svc.cluster.local:8443/keys/current.json"},
		},
		delivered: true,
	},
	{
		// The tenant that adopts the kernel realm: the realm and the tenant
		// differ, and the Secret follows the tenant while the realm objects
		// follow the realm.
		fixture: "tenant-platform", tenant: "platform", realm: "kernel", namespace: "tenant-platform",
		internalURL: "http://gentian-idp-keycloak-keycloakx-http.kernel-authentication.svc.cluster.local:8080/auth",
		components: []vouchingComponent{
			{"herald", "http://herald.tenant-platform.svc.cluster.local:8080/.well-known/jwks.json"},
		},
	},
}

// vouchingIssuersOf are the identity provider entries a render makes under a
// vouching alias, by alias.
func vouchingIssuersOf(objs []map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, o := range ofKind(objs, "IdentityProvider") {
		if alias, _, _ := unstructured.NestedString(o, "spec", "forProvider", "alias"); strings.HasPrefix(alias, VouchingAlias("")) {
			out[alias] = o
		}
	}
	return out
}

// vouchingClientsOf are the clients a render makes under a vouching alias,
// by client id.
func vouchingClientsOf(objs []map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, o := range ofKind(objs, "Client") {
		if id, _, _ := unstructured.NestedString(o, "spec", "forProvider", "clientId"); strings.HasPrefix(id, VouchingAlias("")) {
			out[id] = o
		}
	}
	return out
}

// manifestsNamed are the manifests of one kind that provider-kubernetes is
// handed under one name.
func manifestsNamed(objs []map[string]interface{}, kind, name string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, o := range ofKind(objs, "Object") {
		m, _, _ := unstructured.NestedMap(o, "spec", "forProvider", "manifest")
		if n, _, _ := unstructured.NestedString(m, "metadata", "name"); m["kind"] == kind && n == name {
			out = append(out, m)
		}
	}
	return out
}

// scopesOfClient is the one list of a kind (ClientDefaultScopes,
// ClientOptionalScopes) a render attaches to a client, and whether there is
// one.
func scopesOfClient(objs []map[string]interface{}, kind, field string, client map[string]interface{}) ([]string, int) {
	clientName, _, _ := unstructured.NestedString(client, "metadata", "name")
	var scopes []string
	n := 0
	for _, o := range ofKind(objs, kind) {
		if ref, _, _ := unstructured.NestedString(o, "spec", "forProvider", "clientIdRef", "name"); ref == clientName {
			scopes, _, _ = unstructured.NestedStringSlice(o, "spec", "forProvider", field)
			n++
		}
	}
	return scopes, n
}

// The component as an issuer the realm believes: found by the name its
// statements carry, verified with keys fetched from its own Service, and good
// for the one grant -- nobody signs in through it.
func TestTheVouchingIssuerAsRendered(t *testing.T) {
	for _, c := range vouchingRealmFixtures {
		objs := renderedObjects(t, c.fixture)
		issuers := vouchingIssuersOf(objs)
		if len(issuers) != len(c.components) {
			t.Fatalf("%s: %d vouching identity providers, want one for each of %v", c.fixture, len(issuers), c.components)
		}
		for _, comp := range c.components {
			alias := VouchingAlias(comp.profile)
			idp, ok := issuers[alias]
			if !ok {
				t.Fatalf("%s: no identity provider %q: the registrar would link people to an entry the realm does not hold", c.fixture, alias)
			}
			str := func(field string) string {
				v, _, _ := unstructured.NestedString(idp, "spec", "forProvider", field)
				return v
			}
			// The alias is also what Keycloak calls the entry, which is the
			// external name: a different one would be a second entry.
			if ext, _, _ := unstructured.NestedString(idp, "metadata", "annotations", "crossplane.io/external-name"); ext != alias {
				t.Errorf("%s: %s is known to Keycloak as %q", c.fixture, alias, ext)
			}
			if got := str("realm"); got != c.realm {
				t.Errorf("%s: %s is in realm %q, want %q", c.fixture, alias, got, c.realm)
			}
			// What the component's statements name as iss. Keycloak finds the
			// entry by it, so one letter off is "No Identity Provider for
			// provided issuer".
			if got, want := str("issuer"), VouchingIssuer(c.namespace, comp.profile); got != want {
				t.Errorf("%s: %s has issuer %q, and the component is told %q", c.fixture, alias, got, want)
			}
			if got := str("jwksUrl"); got != comp.jwks {
				t.Errorf("%s: %s fetches its keys from %q, want %q", c.fixture, alias, got, comp.jwks)
			}
			if on, _, _ := unstructured.NestedBool(idp, "spec", "forProvider", "validateSignature"); !on {
				t.Errorf("%s: %s does not verify signatures", c.fixture, alias)
			}
			// Keycloak's identity provider that verifies statements and has
			// no sign-in of its own.
			if got := str("providerId"); got != "jwt-authorization-grant" {
				t.Errorf("%s: %s is a %q", c.fixture, alias, got)
			}
			// The switch itself, and nothing else carried on the side: a key
			// the provider writes and does not read back never converges.
			extra, _, _ := unstructured.NestedStringMap(idp, "spec", "forProvider", "extraConfig")
			if want := map[string]string{"jwtAuthorizationGrantEnabled": "true"}; !reflect.DeepEqual(extra, want) {
				t.Errorf("%s: %s has extraConfig %v, want %v", c.fixture, alias, extra, want)
			}
			for _, f := range []string{"enabled", "hideOnLoginPage", "linkOnly"} {
				if on, _, _ := unstructured.NestedBool(idp, "spec", "forProvider", f); !on {
					t.Errorf("%s: %s does not state %s", c.fixture, alias, f)
				}
			}
			// Stated, because the provider's defaults for two of them are true.
			for _, f := range []string{"storeToken", "trustEmail", "authenticateByDefault", "addReadTokenRoleOnCreate"} {
				if on, has, _ := unstructured.NestedBool(idp, "spec", "forProvider", f); !has || on {
					t.Errorf("%s: %s does not state %s false", c.fixture, alias, f)
				}
			}
			// The two addresses the resource insists on are never asked
			// anything, and must not be somewhere that could answer.
			for _, f := range []string{"authorizationUrl", "tokenUrl"} {
				if got := str(f); got != VouchingIssuer(c.namespace, comp.profile) {
					t.Errorf("%s: %s names %s %q, which is not the issuer's own name", c.fixture, alias, f, got)
				}
			}
			// The credentials it also insists on are not the vouching
			// client's: neither names the component's Secret.
			for _, f := range []string{"clientIdSecretRef", "clientSecretSecretRef"} {
				ref, _, _ := unstructured.NestedStringMap(idp, "spec", "forProvider", f)
				if ref["name"] == "" || ref["name"] == VouchingSecretName(comp.profile) || ref["namespace"] == c.namespace {
					t.Errorf("%s: %s reads its %s from %v", c.fixture, alias, f, ref)
				}
				if len(manifestsNamed(objs, "Secret", ref["name"])) != 1 {
					t.Errorf("%s: %s reads its %s from a Secret %q that is not rendered", c.fixture, alias, f, ref["name"])
				}
			}
			policies, _, _ := unstructured.NestedStringSlice(idp, "spec", "managementPolicies")
			if !reflect.DeepEqual(policies, []string{"Observe", "Create", "Update", "Delete"}) {
				t.Errorf("%s: %s has managementPolicies %v", c.fixture, alias, policies)
			}
			// The people who agreed are linked to this entry, and Keycloak
			// deletes the links with it.
			if p, _, _ := unstructured.NestedString(idp, "spec", "deletionPolicy"); p != "Orphan" {
				t.Errorf("%s: %s has deletionPolicy %q", c.fixture, alias, p)
			}
		}
	}
}

// The client a component asks with can use the one grant, on the strength of
// its own component's statements, and can do nothing else. The token it is
// handed carries no more than the scope that was asked for, and the scopes it
// may ask for are the apps'.
func TestTheVouchingClientAsRendered(t *testing.T) {
	for _, c := range vouchingRealmFixtures {
		if !c.delivered {
			continue
		}
		objs := renderedObjects(t, c.fixture)
		clients := vouchingClientsOf(objs)
		if len(clients) != len(c.components) {
			t.Fatalf("%s: %d vouching clients, want one for each of %v", c.fixture, len(clients), c.components)
		}
		// What the exchange client may ask for is what a vouching client may:
		// the scope of every app of the tenant that has one.
		appScopes, n := scopesOfClient(objs, "ClientOptionalScopes", "optionalScopes", exchangeClientOf(t, c.fixture, objs))
		if n != 1 || len(appScopes) == 0 {
			t.Fatalf("%s: the exchange client has no app scopes to compare with", c.fixture)
		}
		for _, comp := range c.components {
			alias := VouchingAlias(comp.profile)
			client, ok := clients[alias]
			if !ok {
				t.Fatalf("%s: no client %q: the component would ask as a client the realm does not hold", c.fixture, alias)
			}
			if ext, _, _ := unstructured.NestedString(client, "metadata", "annotations", "crossplane.io/external-name"); ext != alias {
				t.Errorf("%s: %s is known to Keycloak as %q", c.fixture, alias, ext)
			}
			if realm, _, _ := unstructured.NestedString(client, "spec", "forProvider", "realmId"); realm != c.realm {
				t.Errorf("%s: %s is in realm %q, want %q", c.fixture, alias, realm, c.realm)
			}
			if access, _, _ := unstructured.NestedString(client, "spec", "forProvider", "accessType"); access != "CONFIDENTIAL" {
				t.Errorf("%s: %s has accessType %q: a client without a secret could be anybody", c.fixture, alias, access)
			}
			for _, flow := range []string{"standardFlowEnabled", "implicitFlowEnabled", "directAccessGrantsEnabled", "serviceAccountsEnabled"} {
				if on, has, _ := unstructured.NestedBool(client, "spec", "forProvider", flow); !has || on {
					t.Errorf("%s: %s does not state %s false: it has a way to a token besides vouching", c.fixture, alias, flow)
				}
			}
			// Vouching is not exchanging: this client holds no token of the
			// person to trade.
			if on, _, _ := unstructured.NestedBool(client, "spec", "forProvider", "standardTokenExchangeEnabled"); on {
				t.Errorf("%s: %s may exchange tokens as well", c.fixture, alias)
			}
			if full, has, _ := unstructured.NestedBool(client, "spec", "forProvider", "fullScopeAllowed"); !has || full {
				t.Errorf("%s: %s does not state fullScopeAllowed false: its tokens would carry every role of the person", c.fixture, alias)
			}
			// The grant, and whose statements it is good for: the component's
			// own identity provider entry and no other.
			attrs, _, _ := unstructured.NestedStringMap(client, "spec", "forProvider", "extraConfig")
			want := map[string]string{
				"oauth2.jwt.authorization.grant.enabled": "true",
				"oauth2.jwt.authorization.grant.idp":     VouchingAlias(comp.profile),
			}
			if !reflect.DeepEqual(attrs, want) {
				t.Errorf("%s: %s has attributes %v, want %v", c.fixture, alias, attrs, want)
			}
			if _, ok := vouchingIssuersOf(objs)[attrs["oauth2.jwt.authorization.grant.idp"]]; !ok {
				t.Errorf("%s: %s is allowed an issuer the realm is not given", c.fixture, alias)
			}
			for _, f := range []string{"validRedirectUris", "rootUrl", "baseUrl", "webOrigins"} {
				if _, has, _ := unstructured.NestedFieldNoCopy(client, "spec", "forProvider", f); has {
					t.Errorf("%s: %s states %s, and nothing is ever redirected to it", c.fixture, alias, f)
				}
			}
			policies, _, _ := unstructured.NestedStringSlice(client, "spec", "managementPolicies")
			if !reflect.DeepEqual(policies, []string{"Observe", "Create", "Update", "Delete"}) {
				t.Errorf("%s: %s has managementPolicies %v", c.fixture, alias, policies)
			}
			// It is pushed the secret the component is told, from the Secret
			// the component reads.
			ref, _, _ := unstructured.NestedStringMap(client, "spec", "forProvider", "clientSecretSecretRef")
			wantRef := map[string]string{"name": VouchingSecretName(comp.profile), "namespace": c.namespace, "key": VouchingClientSecretKey}
			if !reflect.DeepEqual(ref, wantRef) {
				t.Errorf("%s: %s is pushed %v, want the component's own Secret %v", c.fixture, alias, ref, wantRef)
			}

			// One default scope where the provider cannot hold none, as on
			// the exchange client. Anything more is in every token.
			defaults, n := scopesOfClient(objs, "ClientDefaultScopes", "defaultScopes", client)
			if n != 1 || !reflect.DeepEqual(defaults, []string{"basic"}) {
				t.Errorf("%s: %s has default scopes %v in %d lists, want one list holding basic alone", c.fixture, alias, defaults, n)
			}
			// The app scopes, optional: asked for one at a time, and none of
			// the five Keycloak gives every client.
			optional, n := scopesOfClient(objs, "ClientOptionalScopes", "optionalScopes", client)
			if n != 1 || !reflect.DeepEqual(optional, appScopes) {
				t.Errorf("%s: %s has optional scopes %v in %d lists, want the app scopes %v", c.fixture, alias, optional, n, appScopes)
			}
			for _, s := range optional {
				if !strings.HasPrefix(s, AppTokenScope("")) {
					t.Errorf("%s: %s may ask for %q, which is not an app's scope", c.fixture, alias, s)
				}
			}
		}
	}
}

// What the component is told: a Secret of the tenant's own namespace, called
// what the operator says it is called, holding the four entries the operator
// names and nothing else.
func TestTheVouchingSecretIsWhatTheComponentIsTold(t *testing.T) {
	for _, c := range vouchingRealmFixtures {
		if !c.delivered {
			continue
		}
		objs := renderedObjects(t, c.fixture)
		for _, comp := range c.components {
			name := VouchingSecretName(comp.profile)
			found := manifestsNamed(objs, "ExternalSecret", name)
			if len(found) != 1 {
				t.Fatalf("%s: %d ExternalSecrets named %s, want exactly one", c.fixture, len(found), name)
			}
			es := found[0]
			// The tenant's namespace and not the edge's or Keycloak's: the
			// component runs there and a pod reads a Secret of its own
			// namespace only.
			if ns, _, _ := unstructured.NestedString(es, "metadata", "namespace"); ns != c.namespace {
				t.Errorf("%s: %s is written to %q, and the component runs in %q", c.fixture, name, ns, c.namespace)
			}
			if target, _, _ := unstructured.NestedString(es, "spec", "target", "name"); target != name {
				t.Errorf("%s: the Secret is called %q, want %q", c.fixture, target, name)
			}
			// Generated once, and gone with the tenant.
			if p, _, _ := unstructured.NestedString(es, "spec", "refreshPolicy"); p != "CreatedOnce" {
				t.Errorf("%s: %s has refreshPolicy %q: the client's secret would change at an hour nobody chose", c.fixture, name, p)
			}
			if p, _, _ := unstructured.NestedString(es, "spec", "target", "creationPolicy"); p != "Owner" {
				t.Errorf("%s: %s has creationPolicy %q", c.fixture, name, p)
			}

			passwords := manifestsNamed(objs, "Password", name)
			if len(passwords) != 1 {
				t.Fatalf("%s: %d generators named %s, want exactly one", c.fixture, len(passwords), name)
			}
			if ns, _, _ := unstructured.NestedString(passwords[0], "metadata", "namespace"); ns != c.namespace {
				t.Errorf("%s: the generator of %s is in %q: an ExternalSecret reads a generator of its own namespace", c.fixture, name, ns)
			}
			generated, _, _ := unstructured.NestedStringSlice(passwords[0], "spec", "secretKeys")
			if len(generated) != 1 {
				t.Fatalf("%s: the generator of %s writes %v, want one key", c.fixture, name, generated)
			}
			// Whatever number type the decoder chose for it.
			if symbols, has, _ := unstructured.NestedFieldNoCopy(passwords[0], "spec", "symbols"); !has || fmt.Sprint(symbols) != "0" {
				t.Errorf("%s: the generator of %s may write symbols (%v), and the secret travels as a form parameter", c.fixture, name, symbols)
			}

			data, _, _ := unstructured.NestedStringMap(es, "spec", "target", "template", "data")
			want := map[string]string{
				VouchingIssuerKey:   VouchingIssuer(c.namespace, comp.profile),
				VouchingClientIDKey: VouchingAlias(comp.profile),
				// External Secrets fills this one in, from the key the
				// generator writes.
				VouchingClientSecretKey: "{{ ." + generated[0] + " }}",
				// The realm's token endpoint as reached from inside the
				// cluster: the component has no reason to leave it.
				VouchingTokenURLKey: c.internalURL + "/realms/" + c.realm + "/protocol/openid-connect/token",
			}
			if !reflect.DeepEqual(data, want) {
				t.Errorf("%s: %s holds\n  %v\nwant\n  %v", c.fixture, name, data, want)
			}
			// What the component signs as is what the realm looks the issuer
			// up by, and what it asks as is a client the realm holds.
			if idp, ok := vouchingIssuersOf(objs)[data[VouchingClientIDKey]]; !ok {
				t.Errorf("%s: %s names a client id the realm has no issuer under", c.fixture, name)
			} else if iss, _, _ := unstructured.NestedString(idp, "spec", "forProvider", "issuer"); iss != data[VouchingIssuerKey] {
				t.Errorf("%s: %s tells the component it is %q, and the realm knows it as %q", c.fixture, name, data[VouchingIssuerKey], iss)
			}
			if _, ok := vouchingClientsOf(objs)[data[VouchingClientIDKey]]; !ok {
				t.Errorf("%s: %s names a client the realm is not given", c.fixture, name)
			}
		}
	}
}

// The first pass after a component starts vouching, in the tenant that adopts
// the kernel realm: the issuer and the generator, and nothing that needs the
// generated secret to exist already. The sequencer holds the Secret back on
// the generator and the client on the Secret, since a Client is refused
// without the Secret it is pushed. And with no app scope in the realm, no
// list of optional scopes, which the provider cannot hold empty.
func TestAVouchingClientWaitsForItsSecret(t *testing.T) {
	objs := renderedObjects(t, "tenant-platform")
	alias, name := VouchingAlias("herald"), VouchingSecretName("herald")

	if _, ok := vouchingIssuersOf(objs)[alias]; !ok {
		t.Fatalf("no identity provider %q on the first pass", alias)
	}
	if len(manifestsNamed(objs, "Password", name)) != 1 {
		t.Fatalf("no generator %q on the first pass", name)
	}
	if got := manifestsNamed(objs, "ExternalSecret", name); len(got) != 0 {
		t.Error("the Secret is rendered before its generator is ready")
	}
	if clients := vouchingClientsOf(objs); len(clients) != 0 {
		t.Errorf("%d vouching clients are rendered before the Secret they are pushed exists", len(clients))
	}
	for _, o := range ofKind(objs, "ClientOptionalScopes") {
		if ref, _, _ := unstructured.NestedString(o, "spec", "forProvider", "clientIdRef", "name"); strings.Contains(ref, "vouching") {
			t.Errorf("optional scopes are rendered for %s with no app scope in the realm", ref)
		}
	}
}

// A tenant none of whose components vouches is given none of it: no issuer
// the realm believes, no client, no Secret, and not the stand-in credentials
// either.
func TestATenantWithNoVouchingComponentRendersNoneOfIt(t *testing.T) {
	objs := renderedObjects(t, "tenant-single-user")

	if got := vouchingIssuersOf(objs); len(got) != 0 {
		t.Errorf("%d vouching identity providers with no component vouching", len(got))
	}
	// The only identity provider entry such a tenant has is its broker to
	// the kernel realm.
	if got := ofKind(objs, "IdentityProvider"); len(got) != 1 {
		t.Errorf("%d identity providers, want the kernel realm's broker alone", len(got))
	}
	if got := vouchingClientsOf(objs); len(got) != 0 {
		t.Errorf("%d vouching clients with no component vouching", len(got))
	}
	var named []string
	for _, o := range objs {
		resource, _, _ := unstructured.NestedString(o, "metadata", "annotations", "crossplane.io/composition-resource-name")
		if strings.Contains(resource, "vouching") {
			named = append(named, resource)
		}
		m, _, _ := unstructured.NestedMap(o, "spec", "forProvider", "manifest")
		if n, _, _ := unstructured.NestedString(m, "metadata", "name"); strings.HasPrefix(n, VouchingSecretName("")) {
			named = append(named, n)
		}
	}
	sort.Strings(named)
	if len(named) != 0 {
		t.Errorf("rendered with no component vouching: %v", named)
	}
}
