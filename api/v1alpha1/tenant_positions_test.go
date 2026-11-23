/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package v1alpha1

import (
	"strings"
	"testing"
)

func administeringTenant() *Tenant {
	t := &Tenant{}
	t.Name = "shop"
	t.Spec.Positions = []TenantPosition{
		{Name: "customer", Roles: []TenantPositionRole{{App: "shop", Role: "buyer"}}},
		{Name: "owner", Roles: []TenantPositionRole{{Role: "admin"}}},
		{Name: "shopkeeper", Roles: []TenantPositionRole{{App: "shop", Role: "admin"}}},
		{Name: "chief", Includes: []string{"customer", "shopkeeper"}},
		// A circle the director refuses, read without looping.
		{Name: "a", Includes: []string{"b"}},
		{Name: "b", Includes: []string{"a"}},
	}
	return t
}

// A position administers when it holds admin on the tenant or an app,
// itself or through a position it includes; the two administrators'
// positions always do, user never.
func TestPositionAdministers(t *testing.T) {
	tenant := administeringTenant()
	for name, want := range map[string]bool{
		"tenant-administrator": true, "app-administrator": true, "owner": true, "shopkeeper": true, "chief": true,
		"user": false, "customer": false, "a": false, "unknown": false,
	} {
		if got := tenant.PositionAdministers(name); got != want {
			t.Errorf("%s administers: %v, want %v", name, got, want)
		}
	}
}

// Each of the three ways a position is given without an administrator is
// read, and the first that would give administration is named.
func TestAdministrationGivenUnaskedNamesTheWay(t *testing.T) {
	for want, set := range map[string]func(*Tenant){
		"spec.signUp.positions names chief": func(t *Tenant) {
			t.Spec.SignUp = &TenantSignUp{Enabled: true, Positions: []string{"customer", "chief"}}
		},
		"spec.signInFrom (team) names owner": func(t *Tenant) {
			t.Spec.SignInFrom = []TenantSignInFrom{{Tenant: "team", Positions: []string{"owner"}, Holding: []string{"sales"}}}
		},
		"spec.positionGivers (shop/checkout) names shopkeeper": func(t *Tenant) {
			t.Spec.PositionGivers = []TenantPositionGiver{{Tenant: "shop", Component: "checkout", Positions: []string{"shopkeeper"}}}
		},
	} {
		tenant := administeringTenant()
		set(tenant)
		if got := tenant.AdministrationGivenUnasked(); !strings.HasPrefix(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	plain := administeringTenant()
	plain.Spec.SignUp = &TenantSignUp{Enabled: true, Positions: []string{"customer", "user"}}
	if got := plain.AdministrationGivenUnasked(); got != "" {
		t.Errorf("nothing administers, and %q is named", got)
	}
}
