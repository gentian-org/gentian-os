// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCollectGentianTenantGroupNames_IncludesAppAdmins(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantAppSpec{
				{Profile: "nextcloud"},
			},
		},
	}
	names := collectGentianTenantGroupNames(tenant, nil)
	want := []string{
		"gentian:tenant:demo:members",
		"gentian:tenant:demo:admins",
		"gentian:tenant:demo:app-admins",
		"gentian:tenant:demo:app:nextcloud",
	}
	if len(names) != len(want) {
		t.Fatalf("collectGentianTenantGroupNames() = %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("names[%d] = %q, want %q (full=%v)", i, names[i], name, names)
		}
	}
}

func TestGentianTenantAppAdminsGroup(t *testing.T) {
	t.Parallel()
	if got := gentianTenantAppAdminsGroup("demo"); got != "gentian:tenant:demo:app-admins" {
		t.Fatalf("gentianTenantAppAdminsGroup() = %q", got)
	}
}
