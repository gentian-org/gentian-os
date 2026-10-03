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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/branding"
)

// Without a Branding the platform's own brand is published; a Branding
// replaces it; one whose tokens cannot be rendered leaves the published
// brand as it was and says why.
func TestTheBrandIsPublishedAndABrokenOneChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&gentianov1alpha1.Branding{}).Build()
	r := &BrandingReconciler{Client: c}
	published := func() map[string]string {
		t.Helper()
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: brandingConfigMap, Namespace: identityNamespace}, cm); err != nil {
			t.Fatal(err)
		}
		return cm.Data
	}

	if _, err := r.Reconcile(ctx, brandingRequest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(published()[branding.IdentityFile], `"name":"Gentian"`) {
		t.Fatalf("default: %v", published())
	}

	b := &gentianov1alpha1.Branding{}
	b.Name = gentianov1alpha1.BrandingName
	b.Spec.Identity.Name = "Acme Cloud"
	b.Spec.Tokens = &runtime.RawExtension{Raw: []byte(`{"color": {"$type": "color", "brand": {"500": {"$value": "#c0392b"}}}}`)}
	if err := c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, brandingRequest); err != nil {
		t.Fatal(err)
	}
	good := published()
	if !strings.Contains(good[branding.CSSFile], "#c0392b") || !strings.Contains(good[branding.IdentityFile], "Acme Cloud") {
		t.Fatalf("branded: %v", good)
	}

	if err := c.Get(ctx, types.NamespacedName{Name: b.Name}, b); err != nil {
		t.Fatal(err)
	}
	b.Spec.Tokens = &runtime.RawExtension{Raw: []byte(`{"color": {"$type": "color", "brand": {"500": {"$value": "red; }"}}}}`)}
	if err := c.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, brandingRequest); err != nil {
		t.Fatal(err)
	}
	if published()[branding.CSSFile] != good[branding.CSSFile] {
		t.Fatal("a brand that could not be rendered replaced the published one")
	}
	if err := c.Get(ctx, types.NamespacedName{Name: b.Name}, b); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(b.Status.Conditions, "Rendered"); cond == nil || cond.Reason != "TokensInvalid" {
		t.Fatalf("condition = %+v", cond)
	}
}
