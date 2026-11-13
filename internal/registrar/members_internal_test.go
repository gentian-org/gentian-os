/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import "testing"

// The platform tenant has no members group: its people are the kernel
// realm's administrators, and an invitation there names what it names.
func TestThePlatformTenantGetsNoMembersGroup(t *testing.T) {
	got := withMembers(platformTenant, []string{"gentian:platform:admin"})
	if len(got) != 1 || got[0] != "gentian:platform:admin" {
		t.Fatalf("groups = %v", got)
	}
}
