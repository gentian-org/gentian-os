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
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestGroupAttributeNamesDerivesOdooMapper(t *testing.T) {
	t.Parallel()
	// What the Odoo profiles actually produce. Before this was derived, the
	// mapper name was hardcoded in the provisioning script.
	groups := `[
		{"name":"demo-members","attributes":{}},
		{"name":"App Users","attributes":{"gentianOdooGroupRoles":["base.group_user"]}},
		{"name":"App Admins","attributes":{"gentianOdooGroupRoles":["base.group_system"]}}
	]`
	got := groupAttributeNames(groups)
	if len(got) != 1 || got[0] != "gentianOdooGroupRoles" {
		t.Fatalf("got %v, want [gentianOdooGroupRoles]", got)
	}
}

func TestGroupAttributeNamesIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()
	groups := `[
		{"name":"a","attributes":{"zebra":["1"],"alpha":["2"]}},
		{"name":"b","attributes":{"alpha":["3"]}}
	]`
	got := groupAttributeNames(groups)
	if strings.Join(got, ",") != "alpha,zebra" {
		t.Fatalf("got %v, want [alpha zebra] — stable order avoids a churning Job spec", got)
	}
}

func TestGroupAttributeNamesRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	// The name is interpolated into a JSON body inside a shell script, so anything
	// that is not a plain identifier is dropped rather than escaped.
	groups := `[{"name":"a","attributes":{"ok_name":["1"],"bad\"quote":["2"],"has space":["3"],"$(id)":["4"]}}]`
	got := groupAttributeNames(groups)
	if len(got) != 1 || got[0] != "ok_name" {
		t.Fatalf("got %v, want only [ok_name]", got)
	}
}

func TestGroupAttributeNamesToleratesMalformedJSON(t *testing.T) {
	t.Parallel()
	// The groups themselves are provisioned from this same JSON by the script;
	// failing the whole realm job over the mapper list would be worse than a
	// missing claim.
	if got := groupAttributeNames("not json"); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	if got := groupAttributeNames(""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestCollectGentianGroupsJSONIncludesAddonAttributes(t *testing.T) {
	t.Parallel()
	// An Odoo module is an addon, and keycloak-group-attributes lives on the
	// addon profile — the base declares none. Walking only app.Profile dropped
	// gentianOdooGroupRoles from this JSON, and the attribute mapper is derived
	// from it, so the claim never reached Odoo and no module role was assigned.
	base := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "odoo-base-ce"},
	}
	addon := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "odoo-employees-ce",
			Annotations: map[string]string{
				"gentianos.io/keycloak-group-attributes": `{"gentianOdooModules":["hr"],"gentianOdooGroupRoles":["hr.group_hr_user"]}`,
			},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "corp"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{
				Profile: "odoo-base-ce",
				Addons:  []string{"odoo-employees-ce"},
			}},
		},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(base, addon).Build()
	r := &TenantReconciler{Client: c}

	got, err := r.collectGentianGroupsJSON(context.Background(), tenant, nil)
	if err != nil {
		t.Fatalf("collectGentianGroupsJSON: %v", err)
	}
	if !strings.Contains(got, "gentian:tenant:corp:app:odoo-employees-ce") {
		t.Fatalf("addon group missing from groups JSON: %s", got)
	}
	if !strings.Contains(got, "hr.group_hr_user") {
		t.Fatalf("addon attributes missing from groups JSON: %s", got)
	}
	// What the provisioning Job turns into protocol mappers.
	if names := groupAttributeNames(got); strings.Join(names, ",") != "gentianOdooGroupRoles,gentianOdooModules" {
		t.Fatalf("got %v, want [gentianOdooGroupRoles gentianOdooModules]", names)
	}
}
