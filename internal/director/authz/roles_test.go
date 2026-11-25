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
	"testing"
)

// Every cluster relation is held by name, and every role a holder may hold
// is a cluster relation: the two lists cannot drift apart.
func TestEveryClusterRoleIsHeldByName(t *testing.T) {
	byName := map[string]bool{}
	for field, rel := range HolderRoles {
		if !knownClusterRole(rel) {
			t.Errorf("HolderRoles[%q] = %q, which is not a cluster relation", field, rel)
		}
		byName[rel] = true
	}
	for _, rel := range ClusterRoles {
		if !byName[rel] {
			t.Errorf("relation %q is held by nobody's name", rel)
		}
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
		{"admin", PlatformTenant, "cluster", true},
		{"admin", "acme", "cluster", false},
	} {
		scope, ok := HolderScope(c.role, c.tenant)
		if ok != c.ok || (ok && scope != c.scope) {
			t.Errorf("HolderScope(%s, %s) = %q, %v; want %q, %v", c.role, c.tenant, scope, ok, c.scope, c.ok)
		}
	}
}

// A tenant's auditor is a person of the platform tenant, and the one tenant
// they read is the entry's to name; every other role takes its scope from
// HolderScope and may leave it out.
func TestAnEntrysScope(t *testing.T) {
	for _, c := range []struct {
		role, tenant, stated, scope string
		ok                          bool
	}{
		{"tenantAuditor", PlatformTenant, "tenant:acme", "tenant:acme", true},
		{"tenantAuditor", PlatformTenant, "", "", false},
		{"tenantAuditor", PlatformTenant, "cluster", "", false},
		{"tenantAuditor", PlatformTenant, "tenant:platform", "", false},
		{"tenantAuditor", PlatformTenant, "tenant:Acme", "", false},
		{"tenantAuditor", PlatformTenant, "tenant:a/b", "", false},
		{"tenantAuditor", "acme", "tenant:acme", "", false},
		{"perimeterApprover", "acme", "", "tenant:acme", true},
		{"perimeterApprover", "acme", "tenant:acme", "tenant:acme", true},
		{"perimeterApprover", "acme", "tenant:globex", "", false},
		{"auditor", PlatformTenant, "", "cluster", true},
		{"auditor", PlatformTenant, "tenant:acme", "", false},
		{"serviceAdmin", "acme", "cluster", "", false},
	} {
		scope, ok := EntryScope(c.role, c.tenant, c.stated)
		if ok != c.ok || scope != c.scope {
			t.Errorf("EntryScope(%s, %s, %q) = %q, %v; want %q, %v", c.role, c.tenant, c.stated, scope, ok, c.scope, c.ok)
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

func knownClusterRole(rel string) bool {
	for _, r := range ClusterRoles {
		if r == rel {
			return true
		}
	}
	return false
}
