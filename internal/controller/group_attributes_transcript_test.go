/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// groupAttributesAnnotation has every shape a value of the annotation can
// take: a string, a list of mixed items, a number, a boolean, null, an empty
// list and an object.
const groupAttributesAnnotation = `{"s":"x","l":["a",1,true,null,1.5,{"k":"v"}],"n":5,"b":false,"z":null,"e":[],"o":{"k":"v"}}`

// TestGroupAttributesTranscript holds what the groups JSON says for a
// profile's keycloak-group-attributes, and the error a malformed one gives.
func TestGroupAttributesTranscript(t *testing.T) {
	for _, c := range []struct {
		name, annotation, want, wantErr string
	}{
		{"every shape", groupAttributesAnnotation, `[{"name":"gentian:tenant:acme:members","attributes":{}},` +
			`{"name":"gentian:tenant:acme:admins","attributes":{"gentianPartnerRole":["tenant-admin"]}},` +
			`{"name":"gentian:tenant:acme:platform-admins","attributes":{"gentianPartnerRole":["cluster-admin"]}},` +
			`{"name":"gentian:tenant:acme:app:wiki","attributes":{"b":["false"],"e":null,"l":["a","1","true","\u003cnil\u003e","1.5","map[k:v]"],"n":["5"],"o":["map[k:v]"],"s":["x"],"z":["\u003cnil\u003e"]}},` +
			`{"name":"gentian:tenant:acme:app-admins","attributes":{}}]`, ""},
		{"malformed", `{"s":`, "", "failed to unmarshal keycloak-group-attributes for wiki: unexpected end of JSON input"},
	} {
		t.Run(c.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = gentianov1alpha1.AddToScheme(scheme)
			profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{
				Name:        "wiki",
				Annotations: map[string]string{"gentianos.io/keycloak-group-attributes": c.annotation},
			}}
			tenant := &gentianov1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{Name: "acme"},
				Spec:       gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{{Profile: "wiki"}}},
			}
			r := &TenantReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile).Build()}
			got, err := r.collectGentianGroupsJSON(context.Background(), tenant, nil)
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if got != c.want || gotErr != c.wantErr {
				t.Fatalf("got %s (error %q), want %s (error %q)", got, gotErr, c.want, c.wantErr)
			}
		})
	}
}
