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
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestTenantNameFromNamespace(t *testing.T) {
	cases := map[string]string{
		"tenant-demo":      "demo",
		"tenant-acme-corp": "acme-corp",
		"platform-kernel":  "",
		"tenant-":          "",
		"demo":             "",
	}
	for ns, want := range cases {
		if got := tenantNameFromNamespace(ns); got != want {
			t.Errorf("tenantNameFromNamespace(%q) = %q, want %q", ns, got, want)
		}
	}
}

// Job names become pod label values, which Kubernetes caps. A truncation that
// dropped the unit suffix would give two units of the same app one name, and
// the second capture would silently adopt the first one's Job.
func TestExportJobNamesStayUniqueWhenTruncated(t *testing.T) {
	longApp := strings.Repeat("nextcloud-base-edition", 3)

	pg := exportJobName("nightly-2026-08-18", longApp, "pg")
	s3 := exportJobName("nightly-2026-08-18", longApp, "s3")

	if len(pg) > 52 || len(s3) > 52 {
		t.Errorf("names too long: %d, %d", len(pg), len(s3))
	}
	if pg == s3 {
		t.Errorf("truncation collapsed two units onto one name: %q", pg)
	}
	if !strings.HasSuffix(pg, "-pg") || !strings.HasSuffix(s3, "-s3") {
		t.Errorf("truncation dropped the unit suffix: %q / %q", pg, s3)
	}
}

func TestAppStatusIsCreatedOnceAndMutableInPlace(t *testing.T) {
	export := &gentianov1alpha1.TenantExport{}

	first := appStatus(export, "nextcloud")
	first.Phase = gentianov1alpha1.TenantExportPhaseRunning

	second := appStatus(export, "nextcloud")
	if second.Phase != gentianov1alpha1.TenantExportPhaseRunning {
		t.Error("appStatus returned a fresh entry instead of the existing one")
	}
	if len(export.Status.Apps) != 1 {
		t.Errorf("appStatus appended twice: %d entries", len(export.Status.Apps))
	}
}

func TestNextPendingAppWalksInOrderAndStops(t *testing.T) {
	export := &gentianov1alpha1.TenantExport{}
	apps := []string{"a", "b", "c"}

	if got := nextPendingApp(export, apps); got != "a" {
		t.Errorf("first pending = %q, want a", got)
	}

	appStatus(export, "a").Phase = gentianov1alpha1.TenantExportPhaseReady
	if got := nextPendingApp(export, apps); got != "b" {
		t.Errorf("second pending = %q, want b", got)
	}

	appStatus(export, "b").Phase = gentianov1alpha1.TenantExportPhaseReady
	appStatus(export, "c").Phase = gentianov1alpha1.TenantExportPhaseReady
	if got := nextPendingApp(export, apps); got != "" {
		t.Errorf("all done should yield \"\", got %q", got)
	}

	// A failed app is not done: it must not be skipped over silently.
	appStatus(export, "b").Phase = gentianov1alpha1.TenantExportPhaseFailed
	if got := nextPendingApp(export, apps); got != "b" {
		t.Errorf("failed app should still be pending, got %q", got)
	}
}

func TestQuiescedBookkeepingIsIdempotent(t *testing.T) {
	export := &gentianov1alpha1.TenantExport{}

	markQuiesced(export, "a")
	markQuiesced(export, "a")
	if len(export.Status.Quiesced) != 1 {
		t.Errorf("double mark recorded twice: %v", export.Status.Quiesced)
	}

	markQuiesced(export, "b")
	unmarkQuiesced(export, "a")
	if len(export.Status.Quiesced) != 1 || export.Status.Quiesced[0] != "b" {
		t.Errorf("unmark removed the wrong entry: %v", export.Status.Quiesced)
	}

	// Unmarking something never marked must not panic or corrupt the slice —
	// it happens on the failure path when a pause never took effect.
	unmarkQuiesced(export, "never-paused")
	if len(export.Status.Quiesced) != 1 {
		t.Errorf("unexpected mutation: %v", export.Status.Quiesced)
	}
}

func TestIsTerminalCoversBothOutcomes(t *testing.T) {
	cases := map[gentianov1alpha1.TenantExportPhase]bool{
		gentianov1alpha1.TenantExportPhasePending: false,
		gentianov1alpha1.TenantExportPhaseRunning: false,
		gentianov1alpha1.TenantExportPhaseReady:   true,
		gentianov1alpha1.TenantExportPhaseFailed:  true,
		"":                                        false,
	}
	for phase, want := range cases {
		export := &gentianov1alpha1.TenantExport{}
		export.Status.Phase = phase
		if got := export.IsTerminal(); got != want {
			t.Errorf("phase %q: IsTerminal = %v, want %v", phase, got, want)
		}
	}
}

func TestWorkloadMatchingDoesNotReachNeighbours(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		object string
		app    string
		want   bool
	}{
		{"explicit label", map[string]string{"gentianos.io/app": "nextcloud-base-ce"}, "x", "nextcloud-base-ce", true},
		{"instance prefix", map[string]string{"app.kubernetes.io/instance": "nextcloud-base-ce-abc"}, "x", "nextcloud-base-ce", true},
		{"exact name label", map[string]string{"app.kubernetes.io/name": "nextcloud-base-ce"}, "x", "nextcloud-base-ce", true},
		{"object name", nil, "nextcloud-base-ce", "nextcloud-base-ce", true},
		// The volume matcher accepts a family label; scaling must not, because
		// taking a sibling app offline is not recoverable by trying again.
		{"family label alone", map[string]string{"app.kubernetes.io/name": "nextcloud"}, "x", "nextcloud-base-ce", false},
		{"unrelated workload", map[string]string{"app.kubernetes.io/name": "openproject-ce"}, "openproject", "nextcloud-base-ce", false},
	}
	for _, tc := range cases {
		if got := workloadBelongsToApp(tc.labels, tc.object, tc.app); got != tc.want {
			t.Errorf("%s: workloadBelongsToApp = %v, want %v", tc.name, got, tc.want)
		}
	}
}
