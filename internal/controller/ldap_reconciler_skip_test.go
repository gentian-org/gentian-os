/*
Copyright 2026 The Gentian Authors.

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

package controller_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestLDAP_SkippedKeycloakNative(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "ldap-skip"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "LDAP Skip Co",
			Domain:      "ldap-skip.example.com",
			AdminEmail:  "admin@ldap-skip.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "ldap-skip"}, updated)
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == "LDAPReady" &&
				updated.Status.Conditions[i].Status == metav1.ConditionTrue &&
				updated.Status.Conditions[i].Reason == "SkippedKeycloakNative" {
				return true
			}
		}
		return false
	})
}
