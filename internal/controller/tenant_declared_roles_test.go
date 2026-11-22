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
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// The roles a profile declares reach the rights store as it declares them;
// an inclusion of a role it does not declare is dropped rather than stopping
// the tenant's rights from being written.
func TestAProfilesDeclaredRolesAreReadForTheStore(t *testing.T) {
	if got := declaredRoles(nil); got != nil {
		t.Errorf("a profile that declares no roles = %v", got)
	}
	roles := &gentianov1alpha1.ProfileRoles{
		User:  &gentianov1alpha1.ProfileRoleNames{InApp: []string{"sales.salesman"}},
		Admin: &gentianov1alpha1.ProfileRoleNames{InApp: []string{"sales.manager"}},
		Declared: []gentianov1alpha1.ProfileRole{
			{Name: "team-lead", Includes: []string{"reader", "nobody", "team-lead"}, InApp: []string{"sales.lead"}},
			{Name: "reader"},
		},
	}
	want := []authz.DeclaredRole{{Name: "team-lead", Includes: []string{"reader"}}, {Name: "reader"}}
	if got := declaredRoles(roles); !reflect.DeepEqual(got, want) {
		t.Errorf("declared = %+v, want %+v", got, want)
	}
	names := roles.InAppNames()
	wantNames := map[string][]string{"user": {"sales.salesman"}, "admin": {"sales.manager"}, "team-lead": {"sales.lead"}}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("in-app names = %v, want %v: a role with no name inside the app says nothing", names, wantNames)
	}
	if (*gentianov1alpha1.ProfileRoles)(nil).RolesClaim() != "gentian_roles" || roles.RolesClaim() != "gentian_roles" {
		t.Error("the claim's default is gentian_roles")
	}
}
