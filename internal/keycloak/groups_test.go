/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "demo-app"},
			},
		},
	}
	names := CollectTenantGroupNames(tenant, nil)
	want := []string{
		"gentian:tenant:demo:members",
		"gentian:tenant:demo:admins",
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
