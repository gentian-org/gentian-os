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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestXTenantReadyCondition(t *testing.T) {
	t.Parallel()

	xr := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Synced",
					"status":  "True",
					"reason":  "ReconcileSuccess",
					"message": "synced",
				},
				map[string]interface{}{
					"type":    "Ready",
					"status":  string(metav1.ConditionTrue),
					"reason":  "Available",
					"message": "Composite resource is Ready",
				},
			},
		},
	}}

	ready, reason, message := xTenantReadyCondition(xr)
	if !ready {
		t.Fatalf("expected ready=true, got false (reason=%q message=%q)", reason, message)
	}
	if reason != "Available" {
		t.Fatalf("reason = %q, want Available", reason)
	}
	if message != "Composite resource is Ready" {
		t.Fatalf("message = %q", message)
	}
}

func TestXTenantReadyConditionNotReady(t *testing.T) {
	t.Parallel()

	xr := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  string(metav1.ConditionFalse),
					"reason":  "Creating",
					"message": "Unready resources: namespace",
				},
			},
		},
	}}

	ready, reason, message := xTenantReadyCondition(xr)
	if ready {
		t.Fatal("expected ready=false")
	}
	if reason != "Creating" {
		t.Fatalf("reason = %q, want Creating", reason)
	}
	if message != "Unready resources: namespace" {
		t.Fatalf("message = %q", message)
	}
}

func TestXTenantReadyConditionMissing(t *testing.T) {
	t.Parallel()

	xr := &unstructured.Unstructured{Object: map[string]interface{}{}}
	ready, reason, _ := xTenantReadyCondition(xr)
	if ready {
		t.Fatal("expected ready=false")
	}
	if reason != "StatusUnknown" {
		t.Fatalf("reason = %q, want StatusUnknown", reason)
	}
}
