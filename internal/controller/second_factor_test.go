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
