/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1_test

import "testing"

// requires.services.identity.oidc.audiences names what the client's tokens
// carry in aud: the profile's own name, a dash and a label, and never a name
// of the platform's own. profileHead names the profile "x".
func TestTokenAudiencesAreNamesOfTheirOwn(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")

	node := v.schema
	for _, name := range []string{"spec", "requires", "services", "identity", "oidc", "audiences"} {
		next, ok := node.Properties[name]
		if !ok {
			t.Fatalf("the definition has no %s on the way to spec.requires.services.identity.oidc.audiences", name)
		}
		node = &next
	}

	app := "  classes: [app]\n  launch: none\n  trustTier: certified\n  requires:\n    services:\n      identity:\n        oidc:\n          clientId: demo\n"
	own := "is the profile's own"
	for _, c := range []struct{ name, audiences, want string }{
		{"the profile's name and a label", "[x-api]", ""},
		{"two of them", "[x-api, x-store-api]", ""},
		{"the profile's name alone", "[x]", own},
		{"the profile's name and a dash", "[x-]", "should match"},
		{"another name", "[store]", own},
		{"a name that only begins like the profile's", "[xy-api]", own},
		{"a platform service", "[gentian-director]", "may not begin with gentian"},
		{"not a DNS label", "[\"https://store.example\"]", "should match"},
		{"upper case", "[X-Api]", "should match"},
	} {
		expect(t, c.name, v.check(t, profileHead+app+"          audiences: "+c.audiences+"\n", ""), c.want)
	}
}

// requires.services.identity.oidc.requestParameter: a parameter, a redirect
// address computed from it, and a claim. That the parameter and the claim are
// not the protocol's own is the operator's to say (tokenClaimsRefusal).
func TestARequestParameterIsHeldToWhatItMayBe(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")

	app := "  classes: [app]\n  launch: none\n  trustTier: certified\n  requires:\n    services:\n      identity:\n        oidc:\n          clientId: demo\n          requestParameter:\n"
	param := func(name, redirect, claim string) string {
		s := app + "            name: " + name + "\n            pattern: 'https://(?<host>[a-z.]+)'\n            redirectUri: '" + redirect + "'\n"
		if claim != "" {
			s += "            claim: " + claim + "\n"
		}
		return s
	}
	for _, c := range []struct{ name, spec, want string }{
		{"a parameter, its address and its claim", param("tenant_url", "https://store.${host}/oauth/callback", "tenant_url"), ""},
		{"no claim", param("tenant_url", "https://store.${host}/oauth/callback", ""), ""},
		{"an address not computed from it", param("tenant_url", "https://store.example/oauth/callback", ""), "should match"},
		{"an address with a wildcard", param("tenant_url", "https://store.${host}/*", ""), "should match"},
		{"an address with a query", param("tenant_url", "https://store.${host}/cb?x=1", ""), "should match"},
		{"plain http", param("tenant_url", "http://store.${host}/oauth/callback", ""), "should match"},
		{"a parameter not in the protocol's spelling", param("Tenant-URL", "https://store.${host}/oauth/callback", ""), "should match"},
	} {
		expect(t, c.name, v.check(t, profileHead+c.spec, ""), c.want)
	}
}
