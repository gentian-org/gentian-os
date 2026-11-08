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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

const testExtensionGroup = "backup.example.org"

var testScheduleGVK = schema.GroupVersionKind{Group: testExtensionGroup, Version: "v1", Kind: "Schedule"}

// extensionService is a cluster with the backup add-on's Schedule type
// installed as its profile's, granted when granted says so, and schedules
// in two tenants' namespaces.
func extensionService(t *testing.T, granted bool) *Service {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = apiextensionsv1.AddToScheme(scheme)

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "schedules." + testExtensionGroup, Labels: map[string]string{security.APIExtensionLabel: "backup"}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: testExtensionGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "schedules", Kind: "Schedule"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"}},
			}},
		},
	}
	def := crd.DeepCopy()
	def.TypeMeta = metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"}
	def.Labels = nil
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	req := gentianov1alpha1.APIExtensionRequest{
		Name: "schedules", Group: testExtensionGroup, ServiceAccount: "backup",
		Definitions: []runtime.RawExtension{{Raw: raw}}, Reason: "the schedules of the backup add-on",
	}
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "backup"},
		Spec: gentianov1alpha1.ComponentProfileSpec{Requires: &gentianov1alpha1.RequirementSpec{
			Privileges: &gentianov1alpha1.PrivilegeRequest{APIExtensions: []gentianov1alpha1.APIExtensionRequest{req}},
		}},
	}
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: "tenant-platform"},
		Spec:       gentianov1alpha1.ComponentSpec{ProfileRef: gentianov1alpha1.ProfileRef{Name: "backup"}},
	}
	if granted {
		digest, err := security.APIExtensionDigest(&req)
		if err != nil {
			t.Fatal(err)
		}
		comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
			Privilege: "apiExtensions/schedules", Approver: "bea", ApprovedAt: metav1.Now(),
			Reason: "the backup add-on's schedules", Digest: digest,
		}}
	}

	objs := []client.Object{crd, profile, comp}
	for _, s := range []struct{ ns, name, cron string }{
		{"tenant-acme", "weekly", "0 3 * * 0"},
		{"tenant-acme", "nightly", "0 2 * * *"},
		{"tenant-other", "theirs", "0 1 * * *"},
		{"tenant-platform", "cluster", "0 4 * * *"},
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(testScheduleGVK)
		u.SetNamespace(s.ns)
		u.SetName(s.name)
		u.Object["spec"] = map[string]any{"schedule": s.cron}
		u.Object["status"] = map[string]any{"lastRun": "2026-11-05T02:00:00Z"}
		objs = append(objs, u)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(testScheduleGVK, meta.RESTScopeNamespace)
	mapper.Add(gentianov1alpha1.GroupVersion.WithKind("Component"), meta.RESTScopeNamespace)
	mapper.Add(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"), meta.RESTScopeRoot)
	mapper.Add(gentianov1alpha1.GroupVersion.WithKind("ComponentProfile"), meta.RESTScopeRoot)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).Build()
	return &Service{client: c}
}

// A tenant's objects of a granted add-on type are read from its namespace
// alone, with what was declared and what the add-on reports, by name.
func TestAnAddOnsObjectsAreReadFromTheTenantsNamespace(t *testing.T) {
	s := extensionService(t, true)
	out, err := s.ExtensionObjects(context.Background(), "acme", testExtensionGroup, "schedules")
	if err != nil {
		t.Fatal(err)
	}
	if out.Type.Kind != "Schedule" || out.Type.Version != "v1" || out.Type.Profile != "backup" || out.Type.Schema != nil {
		t.Fatalf("type = %+v", out.Type)
	}
	if len(out.Items) != 2 || out.Items[0].Name != "nightly" || out.Items[1].Name != "weekly" {
		t.Fatalf("items = %+v, want acme's nightly and weekly", out.Items)
	}
	if string(out.Items[0].Spec) != `{"schedule":"0 2 * * *"}` || string(out.Items[0].Status) != `{"lastRun":"2026-11-05T02:00:00Z"}` {
		t.Fatalf("item = %s %s", out.Items[0].Spec, out.Items[0].Status)
	}
}

// The cluster's objects are the platform tenant's.
func TestTheClustersAddOnObjectsAreThePlatformTenants(t *testing.T) {
	h := &HTTPServer{Service: extensionService(t, true)}
	mux := http.NewServeMux()
	h.registerExtensionObjectRoutes(&guardedMux{mux: mux, auth: func(next http.HandlerFunc, _ bool) http.HandlerFunc { return next }})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/extensions/"+testExtensionGroup+"/schedules", nil))
	var out ExtensionObjects
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if out.Tenant != gentianov1alpha1.PlatformTenantName || len(out.Items) != 1 || out.Items[0].Name != "cluster" {
		t.Fatalf("answer = %+v", out)
	}
}

// Nothing is read of a type no add-on holds a live grant for, nor of one no
// add-on brought, nor under a name that cannot be one.
func TestOnlyAGrantedAddOnTypeIsRead(t *testing.T) {
	for name, c := range map[string]struct {
		granted                  bool
		tenant, group, resource string
	}{
		"not granted":          {false, "acme", testExtensionGroup, "schedules"},
		"no add-on's":          {true, "acme", "", "pods"},
		"another group":        {true, "acme", "apps", "deployments"},
		"a tenant not a label": {true, "Acme/..", testExtensionGroup, "schedules"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := extensionService(t, c.granted).ExtensionObjects(context.Background(), c.tenant, c.group, c.resource)
			if !errors.Is(err, errNotAnExtension) {
				t.Fatalf("err = %v, want errNotAnExtension", err)
			}
		})
	}
	h := &HTTPServer{Service: extensionService(t, false)}
	mux := http.NewServeMux()
	h.registerExtensionObjectRoutes(&guardedMux{mux: mux, auth: func(next http.HandlerFunc, _ bool) http.HandlerFunc { return next }})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tenants/acme/extensions/"+testExtensionGroup+"/schedules", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an ungranted type: %d, want 404", rec.Code)
	}
}
