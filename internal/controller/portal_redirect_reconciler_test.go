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

package controller

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKernelPortalURL(t *testing.T) {
	got := kernelPortalURL("desk.gentian.org")
	want := "https://portal.desk.gentian.org/login/"
	if got != want {
		t.Fatalf("kernelPortalURL = %q, want %q", got, want)
	}
}

func TestKernelPortalHost(t *testing.T) {
	t.Parallel()
	if got := kernelPortalHost("desk.gentian.org"); got != "portal.desk.gentian.org" {
		t.Fatalf("kernelPortalHost = %q", got)
	}
	if kernelPortalHost("") != "" {
		t.Fatal("expected empty portal host for empty kernel domain")
	}
}

func TestTenantApexRedirectHTTPRoute(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{DisplayName: "Demo"},
	}
	route := buildTenantApexRedirectHTTPRoute(tenant, "tenant-demo", "demo.desk.gentian.org", "desk.gentian.org")
	if route.Name != tenantPortalRedirectName("demo") {
		t.Fatalf("route name = %q", route.Name)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "demo.desk.gentian.org" {
		t.Fatalf("hostnames = %v", route.Spec.Hostnames)
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].Filters) == 0 {
		t.Fatal("expected redirect filter on tenant apex route")
	}
}
