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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func conditionByType(tenant *gentianov1alpha1.Tenant, condType string) metav1.Condition {
	for _, c := range tenant.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	return metav1.Condition{}
}

// A condition that stays False for the same reason still carries new
// information: the error text. Freezing it made a stale message outlive the
// problem it described.
func TestSetCondition_RefreshesMessageWhenStatusAndReasonAreUnchanged(t *testing.T) {
	r := &TenantReconciler{}
	tenant := &gentianov1alpha1.Tenant{}

	r.setCondition(tenant, "AppPrivilegesReady", metav1.ConditionFalse, "SyncFailed", "connection refused")
	r.setCondition(tenant, "AppPrivilegesReady", metav1.ConditionFalse, "SyncFailed", "i/o timeout")

	got := conditionByType(tenant, "AppPrivilegesReady")
	if got.Message != "i/o timeout" {
		t.Fatalf("message not refreshed: got %q, want %q", got.Message, "i/o timeout")
	}
}

// lastTransitionTime answers "how long has this been broken". Reason and
// message churn while a condition legitimately stays False, so only a change of
// status may move it.
func TestSetCondition_LastTransitionTimeTracksStatusOnly(t *testing.T) {
	r := &TenantReconciler{}
	tenant := &gentianov1alpha1.Tenant{}

	r.setCondition(tenant, "AppsReady", metav1.ConditionFalse, "Provisioning", "first")
	first := conditionByType(tenant, "AppsReady").LastTransitionTime

	// Backdate so any rewrite is unmistakable.
	for i := range tenant.Status.Conditions {
		tenant.Status.Conditions[i].LastTransitionTime = metav1.NewTime(first.Add(-time.Hour))
	}
	backdated := conditionByType(tenant, "AppsReady").LastTransitionTime

	// Same status, different reason and message: must not count as a transition.
	r.setCondition(tenant, "AppsReady", metav1.ConditionFalse, "WaitingForApp", "second")
	if got := conditionByType(tenant, "AppsReady"); !got.LastTransitionTime.Equal(&backdated) {
		t.Fatalf("reason change moved lastTransitionTime: got %v, want %v", got.LastTransitionTime, backdated)
	}

	// Status actually changes: now it must move.
	r.setCondition(tenant, "AppsReady", metav1.ConditionTrue, "Provisioned", "ready")
	if got := conditionByType(tenant, "AppsReady"); got.LastTransitionTime.Equal(&backdated) {
		t.Fatalf("status change did not move lastTransitionTime")
	}
}

func TestSetCondition_TracksObservedGeneration(t *testing.T) {
	r := &TenantReconciler{}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Generation = 7

	r.setCondition(tenant, "AppsReady", metav1.ConditionFalse, "Provisioning", "working")
	tenant.Generation = 9
	r.setCondition(tenant, "AppsReady", metav1.ConditionFalse, "Provisioning", "still working")

	if got := conditionByType(tenant, "AppsReady").ObservedGeneration; got != 9 {
		t.Fatalf("observedGeneration not refreshed: got %d, want 9", got)
	}
}

// A fingerprint left behind by an uninstalled app still matches unchanged
// membership, so a reinstall would look already-synced and come back with no
// administrators. Pruning must be driven by what is installed now.
func TestPruneAppPrivilegeFingerprints_ForgetsUninstalledApps(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	tenant.Annotations = map[string]string{
		appPrivilegeSyncAnnotationPrefix + "kept-ce":    "fp-1",
		appPrivilegeSyncAnnotationPrefix + "removed-ce": "fp-2",
		"gentianos.io/unrelated":                        "keep me",
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	r := &TenantReconciler{Client: c}

	if err := r.pruneAppPrivilegeFingerprints(context.Background(), tenant, []string{"kept-ce"}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, ok := tenant.Annotations[appPrivilegeSyncAnnotationPrefix+"removed-ce"]; ok {
		t.Errorf("uninstalled app kept its fingerprint; a reinstall would skip provisioning")
	}
	if tenant.Annotations[appPrivilegeSyncAnnotationPrefix+"kept-ce"] != "fp-1" {
		t.Errorf("installed app lost its fingerprint, forcing a needless re-sync")
	}
	if tenant.Annotations["gentianos.io/unrelated"] != "keep me" {
		t.Errorf("pruning touched an unrelated annotation")
	}
}
