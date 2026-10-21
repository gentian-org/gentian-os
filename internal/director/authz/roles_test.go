/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"reflect"
	"testing"
)

func TestTheClaimsRoleFieldsBecomeTheModelsRelations(t *testing.T) {
	roles, unknown := RolesFromClaim(map[string]string{
		"admin":           "gentian:platform:admin",
		"securityOfficer": "gentian:platform:security",
		"serviceAdmin":    "gentian:platform:service",
		"sharedAppsAdmin": "gentian:platform:shared-apps",
		"breakGlass":      "gentian:platform:break-glass",
		"auditor":         "", // set to nothing grants nothing
	})
	want := map[string]string{
		"admin":             "gentian:platform:admin",
		"security_officer":  "gentian:platform:security",
		"service_admin":     "gentian:platform:service",
		"shared_apps_admin": "gentian:platform:shared-apps",
		"break_glass":       "gentian:platform:break-glass",
	}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %v, want none", unknown)
	}

	// Every translated field is a relation ReconcileClusterRoles accepts, and
	// every relation it accepts has a field: the two lists cannot drift apart.
	for field, rel := range ClaimRoles {
		if !knownClusterRole(rel) {
			t.Errorf("ClaimRoles[%q] = %q, which is not a cluster relation", field, rel)
		}
	}
	for _, rel := range ClusterRoles {
		found := false
		for _, r := range ClaimRoles {
			found = found || r == rel
		}
		if !found {
			t.Errorf("relation %q has no field in the claim", rel)
		}
	}
}

func TestAFieldTheClaimShouldNotHaveIsReportedNotTranslated(t *testing.T) {
	roles, unknown := RolesFromClaim(map[string]string{
		"securityOfficer":  "gentian:platform:security",
		"security_officer": "gentian:platform:security", // the relation's spelling is not a field
		"auditer":          "gentian:platform:auditor",  // a typo
	})
	if want := map[string]string{"security_officer": "gentian:platform:security"}; !reflect.DeepEqual(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if want := []string{"auditer", "security_officer"}; !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
}

func TestARoleHolderIsKeyedByTheRealmThatHoldsTheAccount(t *testing.T) {
	if got, err := HolderUser("test", "6f1c-2a"); err != nil || got != "user:test/6f1c-2a" {
		t.Errorf("HolderUser = %q, %v", got, err)
	}
	for _, bad := range [][2]string{{"", "x"}, {"a/b", "x"}, {"test", "a/b"}, {"test", ""}} {
		if _, err := HolderUser(bad[0], bad[1]); err == nil {
			t.Errorf("HolderUser(%q, %q) should be refused", bad[0], bad[1])
		}
	}
	for field, rel := range HolderRoles {
		if ClaimRoles[field] != rel {
			t.Errorf("HolderRoles[%q] = %q, but the claim's field means %q", field, rel, ClaimRoles[field])
		}
	}
	for _, never := range []string{"admin", "breakGlass"} {
		if _, ok := HolderRoles[never]; ok {
			t.Errorf("%s must not be a role a tenant realm's account holds", never)
		}
	}
}

type checkerFunc func(user string) bool

func (f checkerFunc) Check(_ context.Context, _, user, _, _ string) (bool, error) {
	return f(user), nil
}

func TestCheckAsAsksAsTheHolderOfTheTokensRealmOnlyWhenRefused(t *testing.T) {
	var asked []string
	store := checkerFunc(func(user string) bool {
		asked = append(asked, user)
		return user == "user:test/s1"
	})
	ctx := context.Background()
	if ok, _ := CheckAs(ctx, store, "", "user:s1", "user:test/s1", "can_approve", "cluster:c"); !ok {
		t.Error("the holder of the token's realm should be allowed")
	}
	if ok, _ := CheckAs(ctx, store, "", "user:s1", "user:other/s1", "can_approve", "cluster:c"); ok {
		t.Error("the same subject from another realm must not match the holder")
	}
	if ok, _ := CheckAs(ctx, store, "", "user:s1", "", "can_approve", "cluster:c"); ok {
		t.Error("no realm, no holder")
	}
	asked = nil
	allowed := checkerFunc(func(user string) bool { asked = append(asked, user); return true })
	if ok, _ := CheckAs(ctx, allowed, "", "user:s1", "user:test/s1", "can_view", "tenant:t"); !ok || len(asked) != 1 {
		t.Errorf("an allowed user is asked once, got %v", asked)
	}
}
