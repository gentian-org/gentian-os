/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package webhook

import (
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant whose realm, database prefix or bucket prefix is another
// tenant's is not admitted: the two would have one realm, one set of
// databases and one set of buckets between them, and what one of them
// restored or deleted would be the other's. The director refuses such a
// tenant before it commits it; this is the same rule for a Tenant that
// reaches the cluster by any other way.
func TestANewTenantWhoseNamesAreAnothersIsNotAdmitted(t *testing.T) {
	acme := bareTenant("acme", nil)
	v := gateValidator(t, provenRecord(), &acme)

	for name, isolation := range map[string]*gentianov1alpha1.TenantIsolation{
		"the realm":           {KeycloakRealm: "acme"},
		"the database prefix": {DatabasePrefix: "acme_"},
		"the bucket prefix":   {S3Prefix: "ACME-"},
		"all three, as an import used to write them": {KeycloakRealm: "acme", DatabasePrefix: "acme_", S3Prefix: "acme-"},
	} {
		intruder := bareTenant("acme2", nil)
		intruder.Spec.Isolation = isolation
		resp := handle(t, v, intruder, admissionv1.Create)
		if resp.Allowed {
			t.Errorf("%s: a tenant using acme's was admitted", name)
			continue
		}
		if !strings.Contains(resp.Result.Message, "tenant acme's") || !strings.Contains(resp.Result.Message, `"acme2" is refused`) {
			t.Errorf("%s: the refusal does not say whose: %s", name, resp.Result.Message)
		}
	}

	// Names of its own: admitted, stated or not.
	if resp := handle(t, v, bareTenant("acme2", nil), admissionv1.Create); !resp.Allowed {
		t.Errorf("a tenant with its own names was refused: %s", resp.Result.Message)
	}
	// A tenant that is already there stays manageable, whatever its names.
	existing := bareTenant("acme2", nil)
	existing.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "acme"}
	if resp := handle(t, v, existing, admissionv1.Update); !resp.Allowed {
		t.Errorf("an update of an existing tenant was refused for its names: %s", resp.Result.Message)
	}
	// And a tenant is not its own rival: re-admitting the one that exists.
	if resp := handle(t, v, acme, admissionv1.Create); !resp.Allowed {
		t.Errorf("a tenant was refused for using its own names: %s", resp.Result.Message)
	}
}
