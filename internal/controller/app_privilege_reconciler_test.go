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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/provisioning/privilege"
)

func TestAppPrivilegeSynced(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo",
			Annotations: map[string]string{
				appPrivilegeSyncAnnotationPrefix + "nextcloud": "fp-abc",
			},
		},
	}
	r := &TenantReconciler{}
	if !r.appPrivilegeSynced(tenant, "nextcloud", "fp-abc") {
		t.Fatal("expected synced fingerprint match")
	}
	if r.appPrivilegeSynced(tenant, "nextcloud", "fp-other") {
		t.Fatal("expected fingerprint mismatch")
	}
}

func TestApplyAppPrivilegeReconcileRequest_clearsSyncAnnotations(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo",
			Annotations: map[string]string{
				appPrivilegeRequestedAtAnnotation:              "2026-07-03T12:00:00Z",
				appPrivilegeProcessedAtAnnotation:              "2026-07-02T12:00:00Z",
				appPrivilegeSyncAnnotationPrefix + "nextcloud": "fp-old",
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	r := &TenantReconciler{Client: c}

	if err := r.applyAppPrivilegeReconcileRequest(context.Background(), tenant); err != nil {
		t.Fatalf("applyAppPrivilegeReconcileRequest: %v", err)
	}
	if _, ok := tenant.Annotations[appPrivilegeSyncAnnotationPrefix+"nextcloud"]; ok {
		t.Fatal("expected sync annotation cleared after reconcile request")
	}
}

func TestMemberFingerprint_roleMappingInput(t *testing.T) {
	t.Parallel()
	members := []authz.KeycloakUser{
		{ID: "user-1", Username: "alice"},
		{ID: "user-2", Username: "bob"},
	}
	fp := privilege.MemberFingerprint(members)
	if fp == "" {
		t.Fatal("expected non-empty fingerprint")
	}
	profile := &gentianov1alpha1.AppProfile{
		Spec: gentianov1alpha1.AppProfileSpec{
			Provisioning: &gentianov1alpha1.ProvisioningSpec{
				PrivilegedRole: &gentianov1alpha1.PrivilegedRoleSpec{
					Kind: gentianov1alpha1.PrivilegedRoleKindGroup,
					Name: "admin",
				},
			},
		},
	}
	role := profilePrivilegedRole(profile)
	if role == nil || role.Name != "admin" {
		t.Fatalf("role = %#v", role)
	}
}
