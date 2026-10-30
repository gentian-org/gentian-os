/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// failingExec is an executor whose every command fails: a verification that
// does not pass.
type failingExec struct{ ran [][]string }

func (f *failingExec) Exec(_ context.Context, _, _, _ string, argv []string) (string, error) {
	f.ran = append(f.ran, argv)
	return "no such table", errors.New("exit status 1")
}

// wikiWithVerify is the wiki profile with a verification command.
const wikiWithVerify = wikiBundle + `  backup:
    restore:
      verify: ["true"]
`

// startMove is an instance that ran old, pinned to old, whose profile on the
// cluster has since moved to new (another tenant's install), with old kept as
// a ProfileBuild; then the pin is moved to new.
func startMove(t *testing.T, old, newer string) (*digestHarness, string, string) {
	t.Helper()
	oldDigest, newDigest := profilebundle.Digest([]byte(old)), profilebundle.Digest([]byte(newer))
	h := startDigestHarness(t, materialised(t, old), oldDigest)
	if got := h.reconcile(); componentReadyCondition(got).Reason != "Installing" {
		t.Fatalf("first pass: %+v", componentReadyCondition(got))
	}
	// The release comes up: the first build is recorded as the one it runs.
	h.markReleaseReady()
	got := h.reconcile()
	if ro := got.Status.Rollout; ro == nil || ro.Digest != oldDigest || ro.Phase != gentianov1alpha1.RolloutDone {
		t.Fatalf("after the first rollout: %+v %+v", got.Status.Rollout, componentReadyCondition(got))
	}
	h.events()
	// Another tenant moved the profile; this tenant's build is kept.
	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) {
		m := materialised(t, newer)
		p.Spec, p.Annotations, p.Labels = m.Spec, m.Annotations, m.Labels
	})
	if err := h.c.Create(context.Background(), profileBuildOf(t, old)); err != nil {
		t.Fatal(err)
	}
	h.setPin(newDigest, "2026-10-19T10:00:00Z", "u-ada")
	return h, oldDigest, newDigest
}

func (h *digestHarness) setPin(digest, at, by string) {
	h.t.Helper()
	comp := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), comp); err != nil {
		h.t.Fatal(err)
	}
	comp.Spec.ProfileRef.Digest, comp.Spec.MovedAt, comp.Spec.MovedBy = digest, at, by
	if err := h.c.Update(context.Background(), comp); err != nil {
		h.t.Fatal(err)
	}
}

// markReleaseReady makes the component's Release report ready, as
// provider-helm does once the chart's workloads are up.
func (h *digestHarness) markReleaseReady() {
	h.t.Helper()
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		h.t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(release.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
		map[string]interface{}{"type": "Synced", "status": "True"},
	}, "status", "conditions")
	if err := h.c.Update(context.Background(), release); err != nil {
		h.t.Fatal(err)
	}
	markRenderedReady(h.t, h.c, h.comp)
}

// markReleaseNotReady is the Release during an upgrade: not ready yet.
func (h *digestHarness) markReleaseNotReady() {
	h.t.Helper()
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		h.t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(release.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "message": "upgrading"},
	}, "status", "conditions")
	if err := h.c.Update(context.Background(), release); err != nil {
		h.t.Fatal(err)
	}
}

func (h *digestHarness) export(name string) *gentianov1alpha1.TenantExport {
	h.t.Helper()
	export := &gentianov1alpha1.TenantExport{}
	err := h.c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: h.comp.Namespace}, export)
	if err != nil {
		return nil
	}
	return export
}

func (h *digestHarness) endExport(name string, phase gentianov1alpha1.TenantExportPhase) {
	h.t.Helper()
	export := h.export(name)
	if export == nil {
		h.t.Fatalf("no export %s", name)
	}
	export.Status.Phase = phase
	if phase == gentianov1alpha1.TenantExportPhaseFailed {
		export.Status.Conditions = []metav1.Condition{{Type: "Complete", Status: metav1.ConditionFalse, Reason: "Failed", Message: "the bucket refused the upload"}}
	}
	if err := h.c.Status().Update(context.Background(), export); err != nil {
		h.t.Fatal(err)
	}
}

// A moved pin exports first, keeps the old build running until the export
// is ready, then rolls the new build out and records it once it is up.
func TestAMoveExportsFirstThenRollsOutAndRecordsTheBuild(t *testing.T) {
	newer := strings.ReplaceAll(wikiBundle, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, newDigest := startMove(t, wikiBundle, newer)

	got := h.reconcile()
	ro := got.Status.Rollout
	if ro.Phase != gentianov1alpha1.RolloutExporting || ro.Target != newDigest || ro.Digest != oldDigest || ro.MovedBy != "u-ada" {
		t.Fatalf("after the pin moved: %+v", ro)
	}
	if componentReadyCondition(got).Reason != conditionReasonExporting {
		t.Fatalf("condition = %+v", componentReadyCondition(got))
	}
	export := h.export(ro.Export)
	if export == nil || len(export.Spec.Apps) != 1 || export.Spec.Apps[0] != "wiki" || export.Spec.Destination.Mode != gentianov1alpha1.ExportDestinationPlatform {
		t.Fatalf("export = %+v", export)
	}
	// While it runs, the old build stays.
	h.reconcile()
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("the release moved to %q before the export was ready", h.releasedVersion())
	}

	h.endExport(ro.Export, gentianov1alpha1.TenantExportPhaseReady)
	got = h.reconcile()
	if got.Status.Rollout.Phase != gentianov1alpha1.RolloutRolling || h.releasedVersion() != "2.0.0" {
		t.Fatalf("after the export: phase %s, release %q", got.Status.Rollout.Phase, h.releasedVersion())
	}
	h.markReleaseReady()
	got = h.reconcile()
	ro = got.Status.Rollout
	if ro.Phase != gentianov1alpha1.RolloutDone || ro.Digest != newDigest || ro.Previous != oldDigest || ro.FinishedAt == nil {
		t.Fatalf("after the release came up: %+v", ro)
	}
	if ready := componentReadyCondition(got); ready.Status != "True" {
		t.Fatalf("condition = %+v", ready)
	}
}

// A build whose verification fails is rolled back: the instance is put back
// on the build it ran, the reason is recorded, and nothing retries it until
// a later request asks.
func TestAMoveWhoseVerificationFailsIsRolledBack(t *testing.T) {
	newer := strings.ReplaceAll(wikiWithVerify, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, newDigest := startMove(t, wikiWithVerify, newer)
	exec := &failingExec{}
	h.r.Exec = exec
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "wiki-0", Namespace: h.comp.Namespace,
		Labels: map[string]string{"app.kubernetes.io/name": "wiki", "app.kubernetes.io/instance": "wiki"}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "wiki", Image: "wiki"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if err := h.c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}

	got := h.reconcile()
	h.endExport(got.Status.Rollout.Export, gentianov1alpha1.TenantExportPhaseReady)
	h.reconcile()
	if h.releasedVersion() != "2.0.0" {
		t.Fatalf("release %q while rolling", h.releasedVersion())
	}
	h.markReleaseReady()
	got = h.reconcile()
	ro := got.Status.Rollout
	if ro.Phase != gentianov1alpha1.RolloutRolledBack || ro.Reason != rolloutReasonVerifyFailed || ro.Digest != oldDigest || ro.Target != newDigest {
		t.Fatalf("after a failed verification: %+v", ro)
	}
	if len(exec.ran) != 1 || exec.ran[0][0] != "true" {
		t.Fatalf("verification ran %v", exec.ran)
	}
	// The next pass renders the old build again and says why.
	h.reconcile()
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release %q after the rollback, want the old build", h.releasedVersion())
	}
	h.markReleaseReady()
	got = h.reconcile()
	if ready := componentReadyCondition(got); ready.Reason != conditionReasonRolledBack || !strings.Contains(ready.Message, "no such table") {
		t.Fatalf("condition = %+v", ready)
	}
	// The same request again changes nothing; a later one tries again.
	h.reconcile()
	if got := h.reconcile(); got.Status.Rollout.Phase != gentianov1alpha1.RolloutRolledBack {
		t.Fatalf("retried without a new request: %+v", got.Status.Rollout)
	}
	h.setPin(newDigest, "2026-10-19T11:00:00Z", "u-ada")
	if got := h.reconcile(); got.Status.Rollout.Phase != gentianov1alpha1.RolloutExporting {
		t.Fatalf("a later request did not start a move: %+v", got.Status.Rollout)
	}
}

// An export that fails ends the move before anything changed.
func TestAMoveWhoseExportFailsDoesNotStart(t *testing.T) {
	newer := strings.ReplaceAll(wikiBundle, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, _ := startMove(t, wikiBundle, newer)
	got := h.reconcile()
	h.endExport(got.Status.Rollout.Export, gentianov1alpha1.TenantExportPhaseFailed)
	got = h.reconcile()
	ro := got.Status.Rollout
	if ro.Phase != gentianov1alpha1.RolloutFailed || ro.Reason != rolloutReasonExportFailed || ro.Digest != oldDigest || !strings.Contains(ro.Message, "bucket") {
		t.Fatalf("after a failed export: %+v", ro)
	}
	h.reconcile()
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release %q, want the old build untouched", h.releasedVersion())
	}
}

// A build that does not come up within the deadline is rolled back.
func TestAMoveThatDoesNotComeUpInTimeIsRolledBack(t *testing.T) {
	newer := strings.ReplaceAll(wikiBundle, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, _ := startMove(t, wikiBundle, newer)
	got := h.reconcile()
	h.endExport(got.Status.Rollout.Export, gentianov1alpha1.TenantExportPhaseReady)
	h.markReleaseNotReady()
	got = h.reconcile()
	if got.Status.Rollout.Phase != gentianov1alpha1.RolloutRolling {
		t.Fatalf("not rolling: %+v", got.Status.Rollout)
	}
	// Rolling, and the clock runs out.
	comp := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), comp); err != nil {
		t.Fatal(err)
	}
	late := metav1.NewTime(time.Now().Add(-moveDeadline - time.Minute))
	comp.Status.Rollout.StartedAt = &late
	if err := h.c.Status().Update(context.Background(), comp); err != nil {
		t.Fatal(err)
	}
	got = h.reconcile()
	if ro := got.Status.Rollout; ro.Phase != gentianov1alpha1.RolloutRolledBack || ro.Reason != rolloutReasonNotReadyInTime || ro.Digest != oldDigest {
		t.Fatalf("after the deadline: %+v", ro)
	}
}
