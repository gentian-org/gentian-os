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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestAuthzBridgeReconciler_realmsToSync(t *testing.T) {
	t.Parallel()

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{DisplayName: "Demo"},
	}
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	r := &AuthzBridgeReconciler{
		Client:      fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build(),
		KernelRealm: "kernel",
	}

	t.Run("keycloak-admin secret triggers kernel realm only", func(t *testing.T) {
		t.Parallel()
		realms, err := r.realmsToSync(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: keycloakAdminSecret, Namespace: kernelNamespace},
		})
		if err != nil {
			t.Fatalf("realmsToSync: %v", err)
		}
		if len(realms) != 1 || realms[0] != "kernel" {
			t.Fatalf("realms = %v, want [kernel]", realms)
		}
	})

	t.Run("tenant event syncs tenant realm only", func(t *testing.T) {
		t.Parallel()
		realms, err := r.realmsToSync(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "demo"},
		})
		if err != nil {
			t.Fatalf("realmsToSync: %v", err)
		}
		if len(realms) != 1 || realms[0] != "demo" {
			t.Fatalf("realms = %v, want [demo]", realms)
		}
	})

	t.Run("missing tenant falls back to kernel realm", func(t *testing.T) {
		t.Parallel()
		realms, err := r.realmsToSync(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "missing"},
		})
		if err != nil {
			t.Fatalf("realmsToSync: %v", err)
		}
		if len(realms) != 1 || realms[0] != "kernel" {
			t.Fatalf("realms = %v, want [kernel]", realms)
		}
	})
}
