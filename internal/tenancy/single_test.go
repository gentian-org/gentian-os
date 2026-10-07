/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package tenancy

import (
	"errors"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func tenantNamed(name, realm string) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{}
	t.Name = name
	if realm != "" {
		t.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: realm}
	}
	return t
}

// A single-tenancy cluster carries the platform tenant and exactly one user
// tenant, named user. The platform tenant is not counted under either mode,
// and a multi-tenancy cluster is not limited here at all.
func TestSingleTenancyAdmitsThePlatformTenantAndTheOneUserTenant(t *testing.T) {
	single, multi := gentianov1alpha1.TenancyModeSingle, gentianov1alpha1.TenancyModeMulti
	platform := tenantNamed("platform", "kernel")

	if err := EnforceSingle(single, "kernel", platform); err != nil {
		t.Fatalf("the platform tenant was refused under single: %v", err)
	}
	if err := EnforceSingle(single, "kernel", tenantNamed("user", "")); err != nil {
		t.Fatalf("the one user tenant was refused under single: %v", err)
	}
	// The platform tenant is known by its realm, not its name, and an empty
	// kernel realm means the default one.
	if err := EnforceSingle(single, "", tenantNamed("ops", "kernel")); err != nil {
		t.Fatalf("a platform tenant of another name was refused: %v", err)
	}

	err := EnforceSingle(single, "kernel", tenantNamed("acme", ""))
	if err == nil {
		t.Fatal("a second user tenant was admitted under single")
	}
	if !errors.Is(err, ErrSingleTenancy) {
		t.Fatalf("the refusal does not wrap ErrSingleTenancy: %v", err)
	}
	for _, want := range []string{"tenancy mode is single", `"user"`, `"acme"`, "tenancyMode: multi"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	// A tenant named platform that does not adopt the kernel realm is a user
	// tenant like any other, and is not the one a single cluster carries.
	if err := EnforceSingle(single, "kernel", tenantNamed("platform", "")); err == nil {
		t.Fatal("a user tenant named platform was admitted under single")
	}

	for _, name := range []string{"acme", "user", "beta"} {
		if err := EnforceSingle(multi, "kernel", tenantNamed(name, "")); err != nil {
			t.Fatalf("multi tenancy refused tenant %s: %v", name, err)
		}
	}
}
