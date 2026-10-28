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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCollectTenantGroupNames_IncludesAppAdmins(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "demo-app"},
			},
		},
	}
	names := CollectTenantGroupNames(tenant, nil)
	want := []string{
		"gentian:tenant:demo:members",
		"gentian:tenant:demo:admins",
		"gentian:tenant:demo:platform-admins",
		"gentian:tenant:demo:app-admins",
		"gentian:tenant:demo:app:demo-app",
	}
	if len(names) != len(want) {
		t.Fatalf("CollectTenantGroupNames() = %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("names[%d] = %q, want %q (full=%v)", i, names[i], name, names)
		}
	}
}

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
