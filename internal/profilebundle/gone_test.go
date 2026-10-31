/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func catalogueApplication(syncStatus string) *unstructured.Unstructured {
	app := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"sync": map[string]any{"status": syncStatus},
			"resources": []any{map[string]any{
				"group": gentianov1alpha1.GroupVersion.Group, "kind": KindProfile,
				"name": "app-store-pe", "requiresPruning": true,
			}},
		},
	}}
	app.SetGroupVersionKind(ApplicationGVK)
	app.SetName(CatalogueApplicationPrefix + "prod")
	app.SetNamespace("gitops")
	return app
}

func TestAProfileThatLeftTheCatalogIsGone(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(catalogueApplication("OutOfSync")).Build()
	gone, err := GoneFromCatalogue(context.Background(), reader, "gitops")
	if err != nil || !gone["app-store-pe"] {
		t.Fatalf("gone: %v %v", gone, err)
	}
}

func TestNothingIsGoneWhenTheComparisonFailed(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(catalogueApplication("Unknown")).Build()
	gone, err := GoneFromCatalogue(context.Background(), reader, "gitops")
	if err == nil || gone["app-store-pe"] {
		t.Fatalf("a failed comparison said something is gone: %v %v", gone, err)
	}
}
