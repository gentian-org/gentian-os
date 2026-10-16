/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func orphanScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func trackedTenant(name, application string) *gentianov1alpha1.Tenant {
	return &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{ArgoTrackingAnnotation: application + ":gentianos.io/Tenant:kernel-provisioning/" + name},
	}}
}

func argoApplication(name string) *unstructured.Unstructured {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	app.SetNamespace(layout.Namespace(layout.GitOps))
	app.SetName(name)
	return app
}

// A Tenant whose Application is gone is deleted; one whose Application
// exists, one Argo CD never made, the platform's, and one the cluster does
// not hold are each left alone and refused in their own words.
func TestOnlyATenantWhoseApplicationIsGoneIsRemovedAsAnOrphan(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		objects []client.Object
		tenant  string
		wantErr error
		deleted bool
	}{
		{"its Application is gone: deleted", []client.Object{trackedTenant("user", "tenant-user")}, "user", nil, true},
		{"its Application exists: Argo CD's to prune", []client.Object{trackedTenant("user", "tenant-user"), argoApplication("tenant-user")}, "user", ErrTenantDeclared, false},
		{"no tracking annotation: not Argo CD's", []client.Object{&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "hand"}}}, "hand", ErrNotOrphan, false},
		{"the platform tenant: never", []client.Object{trackedTenant(gentianov1alpha1.PlatformTenantName, "tenant-platform")}, gentianov1alpha1.PlatformTenantName, ErrNotOrphan, false},
	} {
		c := fake.NewClientBuilder().WithScheme(orphanScheme(t)).WithObjects(tc.objects...).Build()
		s := &Service{client: c}
		res, err := s.RemoveOrphanTenant(ctx, tc.tenant, "admin@example")
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		} else if res.Name != tc.tenant || res.Application != "tenant-"+tc.tenant {
			t.Errorf("%s: result %+v", tc.name, res)
		}
		gone := apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: tc.tenant}, &gentianov1alpha1.Tenant{}))
		if gone != tc.deleted {
			t.Errorf("%s: tenant deleted=%v, want %v", tc.name, gone, tc.deleted)
		}
	}

	c := fake.NewClientBuilder().WithScheme(orphanScheme(t)).Build()
	if _, err := (&Service{client: c}).RemoveOrphanTenant(ctx, "nobody", "admin@example"); !apierrors.IsNotFound(err) {
		t.Fatalf("a tenant the cluster does not hold: err = %v, want NotFound", err)
	}
}

// The tenant's state says the custom domain the operator has put it on, and
// why it refused one: an import that keeps a custom domain restores only once
// the tenant is on it.
func TestTheTenantStateSaysItsCustomDomain(t *testing.T) {
	bound := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "bound"}}
	bound.Status.Domain = "acme.example"
	refused := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "refused"}}
	refused.Status.Conditions = []metav1.Condition{{Type: "DomainBound", Status: metav1.ConditionFalse,
		Reason: "DomainTaken", Message: "acme.example is tenant bound's"}}
	c := fake.NewClientBuilder().WithScheme(orphanScheme(t)).WithObjects(bound, refused).Build()
	h := &HTTPServer{Service: &Service{client: c}}
	for name, want := range map[string]TenantState{
		"bound":   {Name: "bound", Exists: true, DeletionPolicy: "Retain", Domain: "acme.example"},
		"refused": {Name: "refused", Exists: true, DeletionPolicy: "Retain", DomainRefused: "acme.example is tenant bound's"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+name, nil)
		req.SetPathValue("tenant", name)
		rec := httptest.NewRecorder()
		h.handleTenantState(rec, req)
		var got TenantState
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got != want {
			t.Errorf("%s: %d %+v (%v), want %+v", name, rec.Code, got, err, want)
		}
	}
}
