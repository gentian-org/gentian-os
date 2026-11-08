/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package schemachecktest builds the cluster side of a definitions check for
// tests: CustomResourceDefinitions that serve what the binary was built with,
// and the same with a field taken out -- which is what an older cluster is.
package schemachecktest

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// Scheme returns a scheme that knows CustomResourceDefinitions.
func Scheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// CRD is the CustomResourceDefinition a current cluster holds for def: every
// version served, with exactly the schema the binary was built with.
func CRD(def schemacheck.Definition) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: def.CRD},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: schemacheck.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: def.Kind},
		},
	}
	for name, schema := range def.Versions {
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name: name, Served: true, Storage: true,
			Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: schema.DeepCopy()},
		})
	}
	return crd
}

// Current is a current cluster's definitions: one CRD per definition in set.
func Current(set *schemacheck.Set) []client.Object {
	out := make([]client.Object, 0, len(set.Definitions))
	for _, def := range set.Definitions {
		out = append(out, CRD(def))
	}
	return out
}

// Kind returns the CRD of one kind from set, current.
func Kind(t testing.TB, set *schemacheck.Set, kind string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	def, ok := set.Kind(kind)
	if !ok {
		t.Fatalf("no definition of %s is embedded", kind)
	}
	return CRD(def)
}

// Without removes the field at path from every version of crd, making it the
// definition of a cluster that predates the field. path is written as the
// check reports it: spec.apps[].addonPins. It fails the test when the field
// is not there to remove, so that a test cannot pass by removing nothing.
func Without(t testing.TB, crd *apiextensionsv1.CustomResourceDefinition, path string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	out := crd.DeepCopy()
	for i := range out.Spec.Versions {
		if !remove(out.Spec.Versions[i].Schema.OpenAPIV3Schema, strings.Split(path, ".")) {
			t.Fatalf("%s has no field %s to take away", crd.Name, path)
		}
	}
	return out
}

func remove(schema *apiextensionsv1.JSONSchemaProps, parts []string) bool {
	name, list := strings.CutSuffix(parts[0], "[]")
	child, ok := schema.Properties[name]
	if !ok {
		return false
	}
	if len(parts) == 1 && !list {
		delete(schema.Properties, name)
		return true
	}
	target := &child
	if list {
		if child.Items == nil || child.Items.Schema == nil {
			return false
		}
		target = child.Items.Schema
	}
	if len(parts) == 1 {
		return false
	}
	if !remove(target, parts[1:]) {
		return false
	}
	schema.Properties[name] = child
	return true
}
