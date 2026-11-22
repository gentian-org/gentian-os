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

	"github.com/gentian-org/gentian-os/internal/authz"
	dauthz "github.com/gentian-org/gentian-os/internal/director/authz"
)

// The mark follows the store: an administrator without it gets it, anybody
// else with it loses it, and an account that is as it should be is left.
func TestTheSecondFactorMarkFollowsWhoAdministers(t *testing.T) {
	marked := map[string][]string{SecondFactorAttribute: {"true"}}
	accounts := []authz.KeycloakUser{
		{ID: "admin-marked", Attributes: marked},
		{ID: "admin-unmarked"},
		{ID: "person-marked", Attributes: marked},
		{ID: "person-unmarked", Attributes: map[string][]string{"uid": {"7"}}},
	}
	got := secondFactorChanges(accounts, map[string]bool{"admin-marked": true, "admin-unmarked": true})
	if len(got) != 2 || got[0].ID != "admin-unmarked" || got[1].ID != "person-marked" {
		t.Errorf("changes = %+v, want the unmarked administrator and the marked person", got)
	}
}

// The cluster's administrators are told from the other realms' holders by
// the realm in the entry.
func TestTheClustersAdministratorsAreAccountsOfThePlatformsRealm(t *testing.T) {
	got := clusterAdministratorsOf([]dauthz.Tuple{
		{User: "user:kernel/a-1", Relation: "admin", Object: "cluster:c1"},
		{User: "user:acme/a-2", Relation: "admin", Object: "cluster:c1"},
		{User: "user:plain", Relation: "admin", Object: "cluster:c1"},
	}, "kernel")
	if len(got) != 1 || got[0] != "a-1" {
		t.Errorf("administrators = %v, want the kernel realm's account alone", got)
	}
}
