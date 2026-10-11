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

// requires.services.identity.sidecar is a field the definition has, and one
// it admits only where what it hands the handler is the app's own.
//
// The fields are asked of the schema rather than by admitting an object: a
// structural schema prunes a field it does not know before any rule runs, so
// a profile declaring one that was never generated would be admitted -- and
// an app would be installed with no way in.
func TestSignInSidecarIsAProfileField(t *testing.T) {
	v := loadCRD(t, "gentianos.io_componentprofiles.yaml")

	sidecar := v.schema
	for _, name := range []string{"spec", "requires", "services", "identity", "sidecar"} {
		next, ok := sidecar.Properties[name]
		if !ok {
			t.Fatalf("the definition has no %s on the way to spec.requires.services.identity.sidecar", name)
		}
		sidecar = &next
	}
	for name, kind := range map[string]string{
		"exposure": "string", "entryPaths": "array", "database": "boolean", "secrets": "array", "appPort": "integer",
	} {
		field, ok := sidecar.Properties[name]
		if !ok {
			t.Fatalf("identity.sidecar has no field %s", name)
		}
		if field.Type != kind {
			t.Fatalf("identity.sidecar.%s is a %q, want %q", name, field.Type, kind)
		}
	}
	// Nothing in it names an address, a client or a Secret: those are the
	// platform's to decide, from where the app answers and what it owns.
	if len(sidecar.Properties) != 5 {
		t.Fatalf("identity.sidecar has %d fields, want the five above: a field that names an address or a Secret would let a profile point the sign-in somewhere else", len(sidecar.Properties))
	}

	app := "  classes: [app]\n  launch: none\n  trustTier: certified\n"
	db := "      database: {engine: postgresql}\n"
	secret := "  secrets:\n    generated:\n      - {name: app_secret, valuePath: appSecret}\n"
	services := "  requires:\n    services:\n"
	for _, c := range []struct{ name, spec, want string }{
		{"declared alone", app + services + "      identity:\n        sidecar: {}\n", ""},
		{"with the app's own database and secret",
			app + secret + services + "      identity:\n        sidecar: {database: true, secrets: [app_secret], appPort: 3000, entryPaths: [/, /login]}\n" + db, ""},
		{"beside an OIDC client", app + services + "      identity:\n        oidc: {clientId: demo}\n        sidecar: {}\n",
			"identity.sidecar signs people in for an app that can do neither OIDC nor SAML itself"},
		{"beside a SAML client", app + services + "      identity:\n        saml: {entityId: demo, acsUrl: https://x}\n        sidecar: {}\n",
			"identity.sidecar signs people in for an app that can do neither OIDC nor SAML itself"},
		{"a database the app does not declare", app + services + "      identity:\n        sidecar: {database: true}\n",
			"identity.sidecar.database needs requires.services.database"},
		{"database said to be off, without one", app + services + "      identity:\n        sidecar: {database: false}\n", ""},
		{"an entry path with a query", app + services + "      identity:\n        sidecar: {entryPaths: ['/login?next=x']}\n", "entryPaths"},
		{"a port that is none", app + services + "      identity:\n        sidecar: {appPort: 70000}\n", "appPort"},
	} {
		expect(t, c.name, v.check(t, profileHead+c.spec, ""), c.want)
	}
}
