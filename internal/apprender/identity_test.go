/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// No fixture observes the pack's client scope, so none renders its mappers.
func TestPackScopeMappersRenderOnceTheScopeIsObserved(t *testing.T) {
	in := &Input{
		App:         "demo-app",
		Namespace:   "tenant-demo",
		ProfileName: "demo-app",
		Profile: &gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "demo"}},
			}},
		},
		Packs: &gentianov1alpha1.OIDCPackCatalogSpec{
			MapperTemplates: map[string]gentianov1alpha1.OIDCMapperTemplate{
				"full_name": {
					KeycloakName:   "full name",
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config:         map[string]string{"claim.name": "name"},
				},
				"groups": {
					ProtocolMapper: "oidc-group-membership-mapper",
					Config:         map[string]string{"introspection.token.claim": "false"},
				},
				"unknown": {},
			},
			Packs: map[string]gentianov1alpha1.OIDCPackSpec{
				"demo": {ScopeName: "demo-scope", Mappers: []string{"full_name", "groups", "unknown", "missing"}},
			},
		},
		Observed: map[string]*unstructured.Unstructured{},
	}
	mappers := func() map[string]*unstructured.Unstructured {
		out := &Rendered{}
		if err := renderIdentity(in, out); err != nil {
			t.Fatal(err)
		}
		m := map[string]*unstructured.Unstructured{}
		for _, obj := range out.Objects {
			if obj.GetKind() == "ProtocolMapper" {
				m[obj.GetName()] = obj
			}
		}
		return m
	}
	if got := mappers(); len(got) != 0 {
		t.Fatalf("mappers rendered before the scope's id is known: %v", got)
	}

	in.Observed["demo-demo-app-keycloak-client-scope"] = &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"atProvider": map[string]interface{}{"id": "1234-uuid"}},
	}}
	got := mappers()
	if len(got) != 2 {
		t.Fatalf("want the two mappers with a template, got %d", len(got))
	}
	full := got["demo-demo-app-scope-mapper-full-name"]
	if full == nil {
		t.Fatal("full_name mapper not rendered under its slug")
	}
	fp, _, _ := unstructured.NestedMap(full.Object, "spec", "forProvider")
	want := map[string]interface{}{
		"realmId":        "demo",
		"name":           "full name",
		"protocol":       "openid-connect",
		"protocolMapper": "oidc-usermodel-attribute-mapper",
		"clientScopeId":  "1234-uuid",
		"config": map[string]interface{}{
			"claim.name":                "name",
			"multivalued":               "false",
			"introspection.token.claim": "true",
		},
	}
	if diff := cmp.Diff(want, fp); diff != "" {
		t.Errorf("full_name mapper (-want +got):\n%s", diff)
	}
	cfg, _, _ := unstructured.NestedStringMap(got["demo-demo-app-scope-mapper-groups"].Object, "spec", "forProvider", "config")
	if diff := cmp.Diff(map[string]string{"introspection.token.claim": "false"}, cfg); diff != "" {
		t.Errorf("groups mapper keeps its own introspection value, no multivalued (-want +got):\n%s", diff)
	}
}
