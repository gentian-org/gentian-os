/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package keycloak

import (
	"testing"
)

func TestTenantAppAdminsGroup(t *testing.T) {
	t.Parallel()
	if got := TenantAppAdminsGroup("demo"); got != "gentian:tenant:demo:app-admins" {
		t.Fatalf("TenantAppAdminsGroup() = %q", got)
	}
}

// The platform's administrators are in it while they are in the tenant
// through the kernel provider; a partner assertion reads cluster-admin off
// it. It is the tenant's, under the tenant's prefix like the admins group,
// so the tenant's realm may speak for it and nothing confuses it with the
// kernel realm's gentian:platform:admin.
func TestTenantPlatformAdminsGroup(t *testing.T) {
	t.Parallel()
	if got := TenantPlatformAdminsGroup("demo"); got != "gentian:tenant:demo:platform-admins" {
		t.Fatalf("TenantPlatformAdminsGroup() = %q", got)
	}
}
