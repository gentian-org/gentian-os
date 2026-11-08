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

// requires.services.mail.imap.tokenSignIn is a field the definition has, and
// one it admits only beside the sign-in client it is about.
//
// The first half is asked of the schema rather than by admitting an object: a
// structural schema prunes a field it does not know before any rule runs, so
// a profile declaring a field that was never generated would be admitted --
// and would silently grant nothing.
func TestMailboxTokenSignInIsAProfileField(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")

	node := v.schema
	for _, name := range []string{"spec", "requires", "services", "mail", "imap", "tokenSignIn"} {
		next, ok := node.Properties[name]
		if !ok {
			t.Fatalf("the definition has no %s on the way to spec.requires.services.mail.imap.tokenSignIn", name)
		}
		node = &next
	}
	if node.Type != "boolean" {
		t.Fatalf("tokenSignIn is a %q, want a boolean: it is said or it is not", node.Type)
	}

	app := "  classes: [app]\n  launch: none\n  trustTier: certified\n  requires:\n    services:\n"
	oidc := "      identity:\n        oidc: {clientId: demo}\n"
	for _, c := range []struct{ name, spec, want string }{
		{"declared, with the client it is about", app + oidc + "      mail:\n        imap: {tokenSignIn: true}\n", ""},
		{"imap alone declares no token sign-in and needs no client", app + "      mail:\n        imap: {}\n", ""},
		{"said to be off, without a client", app + "      mail:\n        imap: {tokenSignIn: false}\n", ""},
		{"declared with no sign-in client", app + "      mail:\n        imap: {tokenSignIn: true}\n", "mail.imap.tokenSignIn needs requires.services.identity.oidc"},
		{"declared beside a SAML client only", app + "      identity:\n        saml: {entityId: demo}\n      mail:\n        imap: {tokenSignIn: true}\n", "mail.imap.tokenSignIn needs requires.services.identity.oidc"},
	} {
		expect(t, c.name, v.check(t, profileHead+c.spec, ""), c.want)
	}
}
