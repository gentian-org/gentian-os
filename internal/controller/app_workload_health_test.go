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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The fingerprint decides whether a refused workload is retried, so what it
// must NOT do is change on its own: a digest that varied with map ordering
// would roll every blocked Deployment on every reconcile.
func TestQuotaFingerprint(t *testing.T) {
	quota := func(cpu, mem string) corev1.ResourceList {
		return corev1.ResourceList{
			corev1.ResourceLimitsCPU:    resource.MustParse(cpu),
			corev1.ResourceLimitsMemory: resource.MustParse(mem),
		}
	}

	a := quotaFingerprint(quota("4", "4Gi"))
	if a != quotaFingerprint(quota("4", "4Gi")) {
		t.Fatal("fingerprint is not stable across calls")
	}
	if a == quotaFingerprint(quota("6", "6Gi")) {
		t.Fatal("a raised quota must not keep the same fingerprint")
	}
	if a == quotaFingerprint(quota("4", "6Gi")) {
		t.Fatal("raising only memory must change the fingerprint")
	}
	if quotaFingerprint(nil) != quotaFingerprint(corev1.ResourceList{}) {
		t.Fatal("absent and empty quotas should agree")
	}
}

// ReplicaFailure is the only signal that a workload was refused; a Deployment
// that is merely still rolling out must not be reported as stuck.
func TestDeploymentBlocked(t *testing.T) {
	withCondition := func(t appsv1.DeploymentConditionType, s corev1.ConditionStatus, msg string) *appsv1.Deployment {
		return &appsv1.Deployment{Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{{Type: t, Status: s, Message: msg}},
		}}
	}

	cases := []struct {
		name string
		dep  *appsv1.Deployment
		want bool
	}{
		{"no conditions", &appsv1.Deployment{}, false},
		{"refused", withCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "exceeded quota"), true},
		{"refusal cleared", withCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionFalse, ""), false},
		{"still progressing", withCondition(appsv1.DeploymentProgressing, corev1.ConditionTrue, ""), false},
		{"available", withCondition(appsv1.DeploymentAvailable, corev1.ConditionTrue, ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := deploymentBlocked(tc.dep)
			if got != tc.want {
				t.Fatalf("deploymentBlocked = %v, want %v", got, tc.want)
			}
			if got && msg == "" {
				t.Fatal("a blocked workload must report why")
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	quota := "pods \"nextcloud-74c\" is forbidden: exceeded quota: tenant-quota\nused: limits.cpu=3700m"
	if got := firstLine(quota); got != "pods \"nextcloud-74c\" is forbidden: exceeded quota: tenant-quota" {
		t.Fatalf("firstLine kept too much: %q", got)
	}
	if got := firstLine("single line"); got != "single line" {
		t.Fatalf("firstLine altered a single line: %q", got)
	}
}

// The retry itself, which had no test.
//
// Three properties matter and each has bitten in production: that a refused
// workload IS retried once the ceiling moves, that it is NOT retried again
// under the same ceiling (a loop would roll every blocked Deployment on every
// reconcile), and that a healthy workload is left entirely alone.
func TestReconcileAppWorkloadHealth_RetryIsBoundedByTheQuota(t *testing.T) {
	t.Parallel()

	const ns = "tenant-retry"

	quota := func(cpu string) *corev1.ResourceQuota {
		return &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: tenantQuotaName, Namespace: ns},
			Spec: corev1.ResourceQuotaSpec{
				Hard: corev1.ResourceList{"limits.cpu": resource.MustParse(cpu)},
			},
		}
	}
	blocked := func(name string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
				Type:    appsv1.DeploymentReplicaFailure,
				Status:  corev1.ConditionTrue,
				Message: "pods \"x\" is forbidden: exceeded quota: tenant-quota\nsecond line",
			}}},
		}
	}
	healthy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fine", Namespace: ns},
	}

	c := fake.NewClientBuilder().WithScheme(SchemeForTest(t)).
		WithObjects(quota("8"), blocked("odoo"), healthy).Build()
	r := &TenantReconciler{Client: c}

	annotationOf := func(name string) string {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, d); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return d.Spec.Template.Annotations[quotaObservedAnnotation]
	}
	// resourceVersion, not the annotation's value: a redundant patch rewrites
	// the SAME fingerprint, so comparing values cannot tell a no-op apart from
	// a write. Only the version moves. (Checked: asserting on the value alone
	// let an unconditional-retry mutation pass.)
	versionOf := func(name string) string {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, d); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return d.ResourceVersion
	}

	// First pass: refused under an 8-CPU ceiling it has not been seen under.
	stuck, err := r.reconcileAppWorkloadHealth(context.Background(), ns)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(stuck) != 1 || !strings.HasPrefix(stuck[0], "odoo ") {
		t.Errorf("expected only odoo reported stuck, got %v", stuck)
	}
	// The message is trimmed to its first line: the API server's refusal carries
	// the whole quota accounting, which does not belong in a status string.
	if strings.Contains(stuck[0], "second line") {
		t.Errorf("stuck message was not trimmed to one line: %q", stuck[0])
	}
	first := annotationOf("odoo")
	if first == "" {
		t.Fatal("blocked deployment was not retried under a new quota")
	}
	if a := annotationOf("fine"); a != "" {
		t.Errorf("healthy deployment was touched: %q", a)
	}

	// Second pass, same ceiling: nothing has changed that would make the retry
	// succeed, so nothing should roll.
	beforeSecond := versionOf("odoo")
	if _, err := r.reconcileAppWorkloadHealth(context.Background(), ns); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := annotationOf("odoo"); got != first {
		t.Errorf("fingerprint changed under an unchanged quota: %q -> %q", first, got)
	}
	if got := versionOf("odoo"); got != beforeSecond {
		t.Errorf("retried again under an unchanged quota: the Deployment was written (%s -> %s)",
			beforeSecond, got)
	}

	// The ceiling moves: this is the operator raising the quota, and the one
	// moment the workload must be given another chance.
	q := &corev1.ResourceQuota{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: tenantQuotaName, Namespace: ns}, q); err != nil {
		t.Fatalf("get quota: %v", err)
	}
	q.Spec.Hard["limits.cpu"] = resource.MustParse("16")
	if err := c.Update(context.Background(), q); err != nil {
		t.Fatalf("raise quota: %v", err)
	}
	if _, err := r.reconcileAppWorkloadHealth(context.Background(), ns); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if got := annotationOf("odoo"); got == first {
		t.Error("quota was raised and the refused workload was not retried")
	}
}
