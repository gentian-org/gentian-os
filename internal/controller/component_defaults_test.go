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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func profileFixture(name string, defaultForTenants bool, classes ...gentianov1alpha1.ComponentClass) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Spec.Classes = classes
	p.Spec.DefaultForTenants = defaultForTenants
	return p
}

// Every profile that declares itself a tenant default becomes a Component in
// the tenant, named after the profile. Nothing is keyed on a name the operator
// knows: the desktop and the administration console are created by the same
// rule, and a third such component needs no change here.
func TestEveryDeclaredDefaultBecomesAComponent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
		profileFixture("admin-console", true, gentianov1alpha1.ComponentClassApp),
		// Declared, but not certified for tenants: there is no tenant to give
		// a shared or system component to, so the declaration is ignored.
		profileFixture("registry", true, gentianov1alpha1.ComponentClassSharedApp),
		// Not declared: an ordinary app a tenant installs on purpose.
		profileFixture("nextcloud", false, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	if err := r.ensureDefaultComponents(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	list := &gentianov1alpha1.ComponentList{}
	if err := c.List(context.Background(), list, client.InNamespace(tenantNamespaceName(tenant))); err != nil {
		t.Fatal(err)
	}
	got := map[string]gentianov1alpha1.Component{}
	for _, comp := range list.Items {
		got[comp.Name] = comp
	}
	if len(got) != 2 {
		t.Fatalf("components = %v, want desktop and admin-console only", keys(got))
	}
	for _, name := range []string{"desktop", "admin-console"} {
		comp, ok := got[name]
		if !ok {
			t.Fatalf("%s was not created", name)
		}
		if comp.Spec.ProfileRef.Name != name || comp.Spec.Class != gentianov1alpha1.ComponentClassApp {
			t.Fatalf("%s spec = %+v", name, comp.Spec)
		}
		// Owned by the tenant, so it goes when the tenant goes.
		if len(comp.OwnerReferences) != 1 || comp.OwnerReferences[0].Name != tenant.Name {
			t.Fatalf("%s owners = %v", name, comp.OwnerReferences)
		}
	}

	// Idempotent: a second pass creates nothing and changes nothing.
	if err := r.ensureDefaultComponents(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	again := &gentianov1alpha1.ComponentList{}
	if err := c.List(context.Background(), again, client.InNamespace(tenantNamespaceName(tenant))); err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != 2 {
		t.Fatalf("second pass: %d components", len(again.Items))
	}
}

// A profile that stops declaring itself a default does not take an existing
// Component with it. Removing a tenant's component is a tenant-level change,
// and this is not where those are decided.
func TestWithdrawingTheDeclarationLeavesExistingComponentsAlone(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	existing := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: tenantNamespaceName(tenant)}}
	existing.Spec.ProfileRef.Name = "desktop"
	existing.Spec.Class = gentianov1alpha1.ComponentClassApp
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), existing,
		profileFixture("desktop", false, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	if err := r.ensureDefaultComponents(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	list := &gentianov1alpha1.ComponentList{}
	if err := c.List(context.Background(), list, client.InNamespace(tenantNamespaceName(tenant))); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "desktop" {
		t.Fatalf("components = %v", list.Items)
	}
}

// A Component written before spec.tenancy became spec.class carries neither.
// The API server hands the stored object back untouched, so the field this
// reconciler reads is empty and the component reports ClassUnsupported for
// ever. It cannot be patched -- class is immutable, and "" to "app" is a
// change -- so the reconciler replaces it.
func TestAComponentWrittenBeforeTheRenameIsReplaced(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	stale := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
		Name: "desktop", Namespace: tenantNamespaceName(tenant),
	}}
	stale.Spec.ProfileRef.Name = "desktop"
	// No Class: what the rename leaves behind.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), stale,
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	if err := r.ensureDefaultComponents(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "desktop", Namespace: tenantNamespaceName(tenant),
	}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Class != gentianov1alpha1.ComponentClassApp {
		t.Fatalf("class = %q, want app: the stale component was not replaced", got.Spec.Class)
	}
}

func keys(m map[string]gentianov1alpha1.Component) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
