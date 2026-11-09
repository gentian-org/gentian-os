/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/layout"
)

func TestTheReleaseRouteAnswersTheClaimsReleaseFields(t *testing.T) {
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "Cluster",
		"metadata": map[string]any{"name": "cluster", "namespace": layout.Namespace(layout.Provisioning)},
		"spec":     map[string]any{"release": map[string]any{"version": "v0.5.1", "autoUpgrade": "patch"}},
		"status": map[string]any{
			"release":  map[string]any{"state": "Applied", "revision": "v0.5.1", "pending": "v0.6.0"},
			"platform": map[string]any{"version": "0.5.1", "build": "0.5.1"},
		},
	}}
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(claim).Build()
	h := &HTTPServer{Service: &Service{client: c}}
	rec := httptest.NewRecorder()
	h.handleRelease(rec, httptest.NewRequest(http.MethodGet, ReleasePath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", ReleasePath, rec.Code, rec.Body.String())
	}
	var view ReleaseView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Spec["version"] != "v0.5.1" || view.Status["pending"] != "v0.6.0" || view.Platform["build"] != "0.5.1" {
		t.Fatalf("view = %+v", view)
	}
	// No claim: said so, not a server error.
	empty := &HTTPServer{Service: &Service{client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()}}
	rec = httptest.NewRecorder()
	empty.handleRelease(rec, httptest.NewRequest(http.MethodGet, ReleasePath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("without a claim = %d", rec.Code)
	}
}
