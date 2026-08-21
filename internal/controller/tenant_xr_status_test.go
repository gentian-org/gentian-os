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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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

// TestTenantFoundationNotReady pins the rule the phase depends on: a Tenant is
// only Ready once every identity and data-plane condition is present AND True.
//
// The absent case is the one that mattered. Finalize decided the phase from
// whether any stage had asked to be requeued, so a Tenant that reconciled
// before its AppProfiles resolved — running the data-plane stage with nothing
// yet to do, and asking for no requeue — reported Ready while carrying none of
// these conditions at all. A check for "no False condition" would have called
// it Ready too.
func TestTenantFoundationNotReady(t *testing.T) {
	t.Parallel()

	all := func(status metav1.ConditionStatus) []metav1.Condition {
		var out []metav1.Condition
		for _, c := range tenantFoundationConditions {
			out = append(out, metav1.Condition{Type: c, Status: status, Reason: "Test"})
		}
		return out
	}

	tests := []struct {
		name  string
		conds []metav1.Condition
		want  string
	}{
		{
			name:  "no conditions at all is not ready",
			conds: nil,
			want:  conditionIdentityReady,
		},
		{
			name:  "every foundation condition True is ready",
			conds: all(metav1.ConditionTrue),
			want:  "",
		},
		{
			name:  "every foundation condition False is not ready",
			conds: all(metav1.ConditionFalse),
			want:  conditionIdentityReady,
		},
		{
			// The regression: conditions absent rather than False, alongside a
			// False AppsReady, and the phase read Ready.
			name: "a missing data-plane condition is not ready",
			conds: []metav1.Condition{
				{Type: conditionIdentityReady, Status: metav1.ConditionTrue, Reason: "Test"},
				{Type: conditionDatabaseReady, Status: metav1.ConditionTrue, Reason: "Test"},
				{Type: conditionMariaDBReady, Status: metav1.ConditionTrue, Reason: "Test"},
				{Type: conditionStorageReady, Status: metav1.ConditionTrue, Reason: "Test"},
				// CacheReady absent.
				{Type: conditionAppsReady, Status: metav1.ConditionFalse, Reason: "Provisioning"},
			},
			want: conditionCacheReady,
		},
		{
			// Deliberately allowed. Finalize logs "tenant ready; apps still
			// converging" and returns their result, so these settle after Ready
			// and must not hold it back — otherwise a Tenant whose apps are
			// still rolling out could never be Ready at all.
			name: "apps, mail and privileges still converging is ready",
			conds: append(all(metav1.ConditionTrue),
				metav1.Condition{Type: conditionAppsReady, Status: metav1.ConditionFalse, Reason: "Provisioning"},
				metav1.Condition{Type: conditionMailReady, Status: metav1.ConditionFalse, Reason: "Provisioning"},
				metav1.Condition{Type: conditionAppPrivilegesReady, Status: metav1.ConditionFalse, Reason: "WaitingForPrerequisites"},
			),
			want: "",
		},
		{
			name: "an unknown status counts as not ready",
			conds: []metav1.Condition{
				{Type: conditionIdentityReady, Status: metav1.ConditionUnknown, Reason: "Test"},
			},
			want: conditionIdentityReady,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tenant := &gentianov1alpha1.Tenant{}
			tenant.Status.Conditions = tc.conds
			if got := tenantFoundationNotReady(tenant); got != tc.want {
				t.Errorf("tenantFoundationNotReady() = %q, want %q", got, tc.want)
			}
		})
	}
}
