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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The operator records a grant and touches no authorization store: the
// director is the store's only writer. A reconcile therefore needs nothing
// but the object, and leaves it Ready with its generation observed.
func TestAppGrantIsRecordedWithoutAStore(t *testing.T) {
	t.Parallel()
	grant := &gentianov1alpha1.AppGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "provider",
			Namespace:  "tenant-demo",
			Labels:     map[string]string{tenantLabel: "demo"},
			Finalizers: []string{appGrantFinalizer},
			Generation: 3,
		},
		Spec: gentianov1alpha1.AppGrantSpec{
			App:     "provider",
			Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "files", Granted: []string{"read"}}},
		},
	}
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).WithStatusSubresource(grant).Build()
	r := &AppGrantReconciler{Client: c}
	key := types.NamespacedName{Name: grant.Name, Namespace: grant.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	updated := &gentianov1alpha1.AppGrant{}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if updated.Status.Phase != gentianov1alpha1.AppGrantPhaseReady {
		t.Fatalf("phase = %q, want Ready", updated.Status.Phase)
	}
	if updated.Status.ObservedGeneration != 3 {
		t.Fatalf("observedGeneration = %d, want 3", updated.Status.ObservedGeneration)
	}
}
