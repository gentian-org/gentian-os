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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
)

type openFGAWriteCapture struct {
	mu      sync.Mutex
	writes  int
	deletes int
}

func (c *openFGAWriteCapture) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/stores/test-store/write" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Writes  []authz.TupleKey `json:"writes"`
		Deletes []authz.TupleKey `json:"deletes"`
	}
	_ = json.Unmarshal(body, &payload)
	c.mu.Lock()
	c.writes += len(payload.Writes)
	c.deletes += len(payload.Deletes)
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func TestAppGrantReconciler_SyncsTupleDiff(t *testing.T) {
	t.Parallel()
	capture := &openFGAWriteCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	grant := &gentianov1alpha1.AppGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider",
			Namespace: "tenant-demo",
			Labels:    map[string]string{tenantLabel: "demo"},
			Finalizers: []string{appGrantFinalizer},
			Annotations: map[string]string{
				syncedTupleKeysAnnotation: `[{"user":"tenant:demo","relation":"tenant","object":"installed_app:demo--provider"}]`,
			},
		},
		Spec: gentianov1alpha1.AppGrantSpec{
			App: "provider",
			Consume: []gentianov1alpha1.ConsumeGrantSpec{
				{Contract: "files", Granted: []string{"read", "write"}},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).WithStatusSubresource(grant).Build()
	r := &AppGrantReconciler{
		Client:     c,
		OpenFGAURL: srv.URL,
		Enabled:    true,
		storeID:    "test-store",
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: grant.Name, Namespace: grant.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.writes == 0 {
		t.Fatal("expected OpenFGA tuple writes")
	}
	if capture.deletes != 0 {
		t.Fatalf("expected no deletes on capability expansion, got %d", capture.deletes)
	}

	updated := &gentianov1alpha1.AppGrant{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: grant.Name, Namespace: grant.Namespace}, updated); err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if updated.Status.Phase != gentianov1alpha1.AppGrantPhaseReady {
		t.Fatalf("phase = %q", updated.Status.Phase)
	}
}

func TestAppGrantReconciler_DeleteRemovesTuples(t *testing.T) {
	t.Parallel()
	capture := &openFGAWriteCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	now := metav1.Now()
	grant := &gentianov1alpha1.AppGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "provider",
			Namespace:         "tenant-demo",
			DeletionTimestamp: &now,
			Finalizers:        []string{appGrantFinalizer},
			Annotations: map[string]string{
				syncedTupleKeysAnnotation: `[{"user":"tenant:demo","relation":"tenant","object":"installed_app:demo--provider"}]`,
			},
		},
		Spec: gentianov1alpha1.AppGrantSpec{App: "provider"},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).Build()
	r := &AppGrantReconciler{
		Client:     c,
		OpenFGAURL: srv.URL,
		Enabled:    true,
		storeID:    "test-store",
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: grant.Name, Namespace: grant.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile delete: %v", err)
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.deletes == 0 {
		t.Fatal("expected OpenFGA tuple deletes on grant removal")
	}
}
