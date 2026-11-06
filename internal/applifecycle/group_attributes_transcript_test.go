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
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// TestGroupAttributesTranscript holds the attributes provisioning an app
// writes on its group for a profile's keycloak-group-attributes: every shape
// a value can take, and a malformed annotation.
func TestGroupAttributesTranscript(t *testing.T) {
	for _, c := range []struct {
		name, annotation, want string
	}{
		{"every shape",
			`{"s":"x","l":["a",1,true,null,1.5,{"k":"v"}],"n":5,"b":false,"z":null,"e":[],"o":{"k":"v"}}`,
			`{"b":["false"],"e":null,"gentianDefaultGrant":["true"],"gentianos.io/managed-by":["platform"],` +
				`"l":["a","1","true","\u003cnil\u003e","1.5","map[k:v]"],"n":["5"],"o":["map[k:v]"],"s":["x"],"z":["\u003cnil\u003e"]}`},
		{"malformed", `{"s":`, `{"gentianDefaultGrant":["true"],"gentianos.io/managed-by":["platform"]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			const group = "gentian:tenant:acme:app:wiki"
			kc := &realm{groups: map[string]map[string][]string{group: {}}, members: map[string][]string{}}
			srv := httptest.NewServer(kc)
			defer srv.Close()
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = gentianov1alpha1.AddToScheme(scheme)
			tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"},
				Spec: gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{KeycloakRealm: "demo"}}}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "keycloak-admin", Namespace: layout.Namespace(layout.Authentication)},
					Data:       map[string][]byte{"url": []byte(srv.URL), "client_id": []byte("admin"), "client_secret": []byte("pw")},
				},
				&gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "wiki",
					Annotations: map[string]string{"gentianos.io/keycloak-group-attributes": c.annotation}}},
				tenant,
			).Build()
			s := &Service{client: cl}
			if err := s.provisionAppGroupUsers(context.Background(), tenant, "wiki"); err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(kc.groups[group])
			if string(got) != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}
