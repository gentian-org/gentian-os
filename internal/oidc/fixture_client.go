// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// NewTestClientWithCatalogFile loads an OIDCPackCatalog YAML file into a fake client.
// Intended for unit tests in other packages.
func NewTestClientWithCatalogFile(t *testing.T, path string) client.Client {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var catalog gentianov1alpha1.OIDCPackCatalog
	if err := yaml.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(&catalog).Build()
}
