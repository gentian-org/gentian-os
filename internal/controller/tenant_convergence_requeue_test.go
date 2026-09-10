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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// tenantStalledFor builds a Tenant whose most recent condition transition is
// `age` old, which is the signal the pacing reads.
func tenantStalledFor(age time.Duration, now time.Time) *gentianov1alpha1.Tenant {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Status.Conditions = []metav1.Condition{
		{
			Type:               conditionNamespaceReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(now.Add(-age - time.Hour)),
		},
		{
			Type:               conditionCrossplaneReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(now.Add(-age)),
		},
	}
	return tenant
}

func TestTenantConvergenceRequeuePacing(t *testing.T) {
	now := time.Now()
	base := 2 * time.Second

	cases := []struct {
		name  string
		since time.Duration
		want  time.Duration
	}{
		// Something that just moved is most likely about to move again, so the
		// stage's own pace is kept.
		{"just transitioned", 5 * time.Second, base},
		{"still inside the fast window", 55 * time.Second, base},
		// Past a minute with nothing moving, polling this hard buys nothing.
		{"stalled a couple of minutes", 2 * time.Minute, 8 * time.Second},
		{"stalled past the slow window", 10 * time.Minute, 30 * time.Second},
		// base*15 would be 45s here; the cap is what keeps a long wait from
		// turning into a long silence.
		{"capped, not unbounded", 3 * time.Hour, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tenantConvergenceRequeue(tenantStalledFor(tc.since, now), base, now)
			if got != tc.want {
				t.Errorf("stalled %s: requeue %s, want %s", tc.since, got, tc.want)
			}
		})
	}
}

// A stage that asked for no requeue is asking not to be run again on a timer.
// Scaling zero into something non-zero would invent a poll nothing requested.
func TestTenantConvergenceRequeueLeavesZeroAlone(t *testing.T) {
	now := time.Now()
	if got := tenantConvergenceRequeue(tenantStalledFor(time.Hour, now), 0, now); got != 0 {
		t.Errorf("requeue %s, want 0", got)
	}
}

// A tenant with no transitions yet has only just arrived. Backing it off on the
// strength of a zero timestamp would treat brand new as long stuck - the exact
// inversion of what the pacing is for.
func TestTenantConvergenceRequeueKeepsPaceForNewTenant(t *testing.T) {
	now := time.Now()
	tenant := &gentianov1alpha1.Tenant{}
	if got := tenantConvergenceRequeue(tenant, 2*time.Second, now); got != 2*time.Second {
		t.Errorf("requeue %s, want 2s", got)
	}
}

// The most recent transition is what counts. Reading the first, or the last in
// slice order, would report a tenant as stalled while another of its conditions
// was still moving.
func TestTenantConvergenceRequeueUsesMostRecentTransition(t *testing.T) {
	now := time.Now()
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Status.Conditions = []metav1.Condition{
		{
			Type:               conditionCrossplaneReady,
			LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Hour)),
		},
		{
			Type:               conditionIdentityReady,
			LastTransitionTime: metav1.NewTime(now.Add(-3 * time.Second)),
		},
		{
			Type:               conditionNamespaceReady,
			LastTransitionTime: metav1.NewTime(now.Add(-90 * time.Minute)),
		},
	}
	if got := tenantConvergenceRequeue(tenant, 2*time.Second, now); got != 2*time.Second {
		t.Errorf("requeue %s, want 2s: one condition moved three seconds ago", got)
	}
}
