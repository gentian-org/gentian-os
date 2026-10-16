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
// carry in aud: one DNS label each, and never a name of the platform's own.
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
	for _, c := range []struct{ name, audiences, want string }{
		{"a service's name", "[store]", ""},
		{"two names", "[store, billing-api]", ""},
		{"a platform service", "[gentian-director]", "may not begin with gentian"},
		{"the platform's prefix alone", "[gentian]", "may not begin with gentian"},
		{"not a DNS label", "[\"https://store.example\"]", "should match"},
		{"upper case", "[Store]", "should match"},
	} {
		expect(t, c.name, v.check(t, profileHead+app+"          audiences: "+c.audiences+"\n", ""), c.want)
	}
}
