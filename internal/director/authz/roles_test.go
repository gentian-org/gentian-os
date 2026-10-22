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
		"admin":      "gentian:platform:admin",
		"breakGlass": "gentian:platform:break-glass",
	})
	want := map[string]string{
		"admin":       "gentian:platform:admin",
		"break_glass": "gentian:platform:break-glass",
	}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %v, want none", unknown)
	}

	// Every relation a group or a holder holds is one ReconcileClusterRoles
	// accepts, and every one it accepts is held one way or the other, never
	// both: the lists cannot drift apart.
	for field, rel := range ClaimRoles {
		if !knownClusterRole(rel) {
			t.Errorf("ClaimRoles[%q] = %q, which is not a cluster relation", field, rel)
		}
		if _, both := HolderRoles[field]; both {
			t.Errorf("%s is held by a group and by name", field)
		}
	}
	for _, rel := range ClusterRoles {
		found := 0
		for _, r := range ClaimRoles {
			if r == rel {
				found++
			}
		}
		for _, r := range HolderRoles {
			if r == rel {
				found++
			}
		}
		if found != 1 {
			t.Errorf("relation %q is held %d ways", rel, found)
		}
	}
}

func TestAFieldTheClaimShouldNotHaveIsReportedNotTranslated(t *testing.T) {
	roles, unknown := RolesFromClaim(map[string]string{
		"admin":           "gentian:platform:admin",
		"securityOfficer": "gentian:platform:security", // held by name, not by a group
		"break_glass":     "gentian:platform:x",        // the relation's spelling is not a field
		"auditer":         "gentian:platform:auditor",  // a typo
	})
	if want := map[string]string{"admin": "gentian:platform:admin"}; !reflect.DeepEqual(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if want := []string{"auditer", "break_glass", "securityOfficer"}; !reflect.DeepEqual(unknown, want) {
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
	for _, rel := range HolderRoles {
		if !knownClusterRole(rel) {
			t.Errorf("%q is not a cluster relation", rel)
		}
	}
	for _, never := range []string{"admin", "breakGlass"} {
		if _, ok := HolderRoles[never]; ok {
			t.Errorf("%s must not be a role held by name", never)
		}
	}
}

// How far a role reaches follows from the role and the holder's tenant.
func TestARoleHoldersScopeFollowsTheRoleAndTheTenant(t *testing.T) {
	for _, c := range []struct {
		role, tenant, scope string
		ok                  bool
	}{
		{"securityOfficer", "acme", "cluster", true},
		{"securityOfficer", PlatformTenant, "cluster", true},
		{"auditor", "acme", "cluster", true},
		{"serviceAdmin", PlatformTenant, "cluster", true},
		{"serviceAdmin", "acme", "cluster", false},
		{"sharedAppsAdmin", "acme", "cluster", false},
		{"perimeterApprover", PlatformTenant, "cluster", true},
		{"perimeterApprover", "acme", "tenant:acme", true},
		{"admin", PlatformTenant, "", false},
	} {
		scope, ok := HolderScope(c.role, c.tenant)
		if ok != c.ok || (ok && scope != c.scope) {
			t.Errorf("HolderScope(%s, %s) = %q, %v; want %q, %v", c.role, c.tenant, scope, ok, c.scope, c.ok)
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
