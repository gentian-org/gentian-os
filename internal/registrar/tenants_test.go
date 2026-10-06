/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func tenant(name string, edit func(*gentianov1alpha1.Tenant)) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if edit != nil {
		edit(t)
	}
	return t
}

// What the routes need about a tenant is read off the Tenant object, by the
// rules the director applied to the manifest in git.
func TestATenantsRealmAndLoginDomainComeFromTheCluster(t *testing.T) {
	no := false
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		// A realm to itself, as the operator reports it.
		tenant("acme", func(t *gentianov1alpha1.Tenant) {
			t.Status.AdminEmail = "admin@acme.k.example"
		}),
		// The platform tenant adopts the kernel realm: its people sign in
		// under the kernel domain, whatever address the operator derives.
		tenant("platform", func(t *gentianov1alpha1.Tenant) {
			t.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
			t.Status.AdminEmail = "admin@platform.k.example"
		}),
		// A custom domain the operator accepted.
		tenant("vanity", func(t *gentianov1alpha1.Tenant) {
			t.Status.Domain = "vanity.example"
			t.Status.AdminEmail = "admin@vanity.example"
			t.Spec.Admin = &gentianov1alpha1.TenantAdmin{RequireMFA: &no}
		}),
		// Single tenancy: the operator resolved the kernel domain itself.
		tenant("solo", func(t *gentianov1alpha1.Tenant) {
			t.Status.AdminEmail = "admin@k.example"
		}),
		// Not reconciled yet, and one the operator knows no domain for.
		tenant("fresh", nil),
		tenant("lost", func(t *gentianov1alpha1.Tenant) {
			t.Status.AdminEmail = "admin@lost.invalid"
		}),
	).Build()
	tenants := &registrar.ClusterTenants{Client: c, KernelDomain: "k.example"}

	for name, want := range map[string]registrar.Tenant{
		"acme":     {Name: "acme", Realm: "acme", LoginDomain: "acme.k.example", AdminRequiresMFA: true},
		"platform": {Name: "platform", Realm: "kernel", LoginDomain: "k.example", AdminRequiresMFA: true},
		"vanity":   {Name: "vanity", Realm: "vanity", LoginDomain: "vanity.example", AdminRequiresMFA: false},
		"solo":     {Name: "solo", Realm: "solo", LoginDomain: "k.example", AdminRequiresMFA: true},
		"fresh":    {Name: "fresh", Realm: "fresh", LoginDomain: "", AdminRequiresMFA: true},
		"lost":     {Name: "lost", Realm: "lost", LoginDomain: "", AdminRequiresMFA: true},
	} {
		got, err := tenants.Tenant(context.Background(), name)
		if err != nil || got != want {
			t.Errorf("%s = %+v, %v; want %+v", name, got, err, want)
		}
	}

	if _, err := tenants.Tenant(context.Background(), "nowhere"); !errors.Is(err, registrar.ErrTenantNotFound) {
		t.Errorf("a tenant the cluster does not have: %v, want ErrTenantNotFound", err)
	}
	if _, err := tenants.Tenant(context.Background(), "../kernel"); err == nil {
		t.Error("a name that is not a tenant name was looked up")
	}

	all, err := tenants.All(context.Background())
	if err != nil || len(all) != 6 || all[0].Name != "acme" {
		t.Errorf("All = %+v, %v", all, err)
	}
}

func claim(name, namespace string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "gentianos.io", Version: "v1alpha1", Kind: "Cluster"})
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// The group the registrar protects is the one the Cluster claim names as the
// platform's administrators: the field the operator projects into the store.
func TestTheAdministratorsGroupIsReadFromTheClusterClaim(t *testing.T) {
	ns := layout.Namespace(layout.Provisioning)
	named := map[string]any{"platformRoles": map[string]any{"admin": "acme:operators", "auditor": "acme:audit"}}

	for name, c := range map[string]struct {
		objects []client.Object
		want    []string
	}{
		"the claim names it":     {[]client.Object{claim("main", ns, named)}, []string{"acme:operators"}},
		"the claim names none":   {[]client.Object{claim("main", ns, map[string]any{})}, []string{"gentian:platform:admin"}},
		"no claim yet":           {nil, []string{"gentian:platform:admin"}},
		"a claim somewhere else": {[]client.Object{claim("stray", "default", named)}, []string{"gentian:platform:admin"}},
		"written as a path":      {[]client.Object{claim("main", ns, map[string]any{"platformRoles": map[string]any{"admin": "/acme:operators"}})}, []string{"acme:operators"}},
	} {
		reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(c.objects...).Build()
		got, err := registrar.ClaimAdminGroups(reader)(context.Background())
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, %v; want %v", name, got, err, c.want)
		}
	}
}

// A claim that cannot be read names nothing, and that is an error rather than
// the default: the caller refuses the write instead of guessing the group.
func TestAnUnreadableClaimIsAnErrorNotADefault(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("the API server did not answer")
		},
	}).Build()
	if got, err := registrar.ClaimAdminGroups(reader)(context.Background()); err == nil {
		t.Fatalf("got %v and no error", got)
	}
}
