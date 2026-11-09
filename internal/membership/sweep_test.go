/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package membership_test

import (
	"testing"

	"github.com/gentian-org/gentian-os/internal/membership"
)

// A person removed from Keycloak whose deletion statement never arrived keeps
// the memberships the last statement about them gave.
func TestALostDeletionLeavesTheMembershipsInPlace(t *testing.T) {
	store := newStore(t)
	l := start(t, store)
	l.send(membership.Event{Realm: "demo", Type: membership.TypeUserMemberships, User: "u-gone",
		Groups: []string{"gentian:tenant:demo:admins"}})

	// u-gone is deleted in Keycloak; the statement saying so is lost.

	if got := groupsOf(t, store, "u-gone"); got != "gentian/tenant/demo/admins" {
		t.Fatalf("groups = %q", got)
	}
}
