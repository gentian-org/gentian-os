/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A credential a person supplied for an app is not in a bundle: the issuer
// has it, and a restore asks for it again (security.md §6.4). The generated
// secret beside it is, as before.
func TestASuppliedCredentialIsNotAmongTheSecretsABundleHolds(t *testing.T) {
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{
		Generated: []gentianov1alpha1.AppSecret{{Name: "admin_password", ValuePath: "admin.password"}},
		Supplied: []gentianov1alpha1.SuppliedSecret{{
			Name: "payments", DisplayName: "Payment provider keys",
			Fields: []gentianov1alpha1.SuppliedField{{Key: "secret_key", ValuePath: "payments.secretKey"}},
		}},
	}
	got := declaredSecrets("shop", profile)
	if len(got) != 1 || got[0].name != "admin_password" {
		names := make([]string, 0, len(got))
		for _, d := range got {
			names = append(names, d.name)
		}
		t.Fatalf("declared secrets = %v, want only admin_password", names)
	}
}
