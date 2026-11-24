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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// recordingExec is an executor whose every command passes.
type recordingExec struct{ ran [][]string }

func (f *recordingExec) Exec(_ context.Context, _, _, _ string, argv []string) (string, error) {
	f.ran = append(f.ran, argv)
	return "", nil
}

// wikiWithData is the wiki profile with data of its own, a volume: what a
// move exports first.
const wikiWithData = wikiBundle + `  backup:
    volumes:
      include: [wiki-data]
`

// wikiWithVerify is the wiki profile with data and a verification command.
const wikiWithVerify = wikiWithData + `    restore:
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
// provider-helm does once it has deployed what the Release says: a new Helm
// revision, its workloads up.
func (h *digestHarness) markReleaseReady() {
	h.t.Helper()
	h.reportRelease(true)
}

// markReleaseStillReady is the Release right after a change was written to
// it, before provider-helm has deployed it: its conditions still say the
// revision before is ready, and the revision has not moved.
func (h *digestHarness) markReleaseStillReady() {
	h.t.Helper()
	h.reportRelease(false)
}

func (h *digestHarness) reportRelease(deployed bool) {
	h.t.Helper()
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		h.t.Fatal(err)
	}
	if deployed {
		_ = unstructured.SetNestedField(release.Object, releaseRevision(release)+1, "status", "atProvider", "revision")
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
	newer := strings.ReplaceAll(wikiWithData, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, newDigest := startMove(t, wikiWithData, newer)

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
	// Unless a policy says otherwise, the app alone.
	if export.CapturesTenantCore() {
		t.Fatalf("the export captures the tenant's own: %+v", export.Spec)
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

// A Release keeps reporting the revision before as ready until provider-helm
// has deployed what was written to it. A move is not done on that report:
// it waits for a later Helm revision, however ready the old pods look.
func TestAMoveIsNotDoneBeforeHelmHasDeployedTheNewBuild(t *testing.T) {
	newer := strings.ReplaceAll(wikiWithData, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, newDigest := startMove(t, wikiWithData, newer)

	got := h.reconcile()
	h.endExport(got.Status.Rollout.Export, gentianov1alpha1.TenantExportPhaseReady)
	got = h.reconcile()
	if got.Status.Rollout.Phase != gentianov1alpha1.RolloutRolling || h.releasedVersion() != "2.0.0" {
		t.Fatalf("after the export: phase %s, release %q", got.Status.Rollout.Phase, h.releasedVersion())
	}

	// The new build is written; the Release still says the old one is up.
	h.markReleaseStillReady()
	markRenderedReady(t, h.c, h.comp)
	got = h.reconcile()
	if ro := got.Status.Rollout; ro.Phase != gentianov1alpha1.RolloutRolling || ro.Digest != oldDigest {
		t.Fatalf("the move was taken as done before Helm deployed it: %+v", ro)
	}
	if msg := componentReadyCondition(got).Message; !strings.Contains(msg, "waiting for the chart to deploy") {
		t.Fatalf("condition = %+v", componentReadyCondition(got))
	}

	// Helm deploys it: a later revision.
	h.markReleaseReady()
	got = h.reconcile()
	if ro := got.Status.Rollout; ro.Phase != gentianov1alpha1.RolloutDone || ro.Digest != newDigest {
		t.Fatalf("after Helm deployed the new build: %+v", ro)
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
	newer := strings.ReplaceAll(wikiWithData, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, _ := startMove(t, wikiWithData, newer)
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

// A later request after a failed export exports afresh: it does not stop on
// the failure of the attempt before, which stays as its record.
func TestARetriedMoveTakesAFreshExport(t *testing.T) {
	newer := strings.ReplaceAll(wikiWithData, `version: "1.0.0"`, `version: "2.0.0"`)
	h, _, newDigest := startMove(t, wikiWithData, newer)
	got := h.reconcile()
	failed := got.Status.Rollout.Export
	h.endExport(failed, gentianov1alpha1.TenantExportPhaseFailed)
	if got = h.reconcile(); got.Status.Rollout.Phase != gentianov1alpha1.RolloutFailed {
		t.Fatalf("after a failed export: %+v", got.Status.Rollout)
	}

	h.setPin(newDigest, "2026-10-19T11:00:00Z", "u-ada")
	got = h.reconcile()
	ro := got.Status.Rollout
	if ro.Phase != gentianov1alpha1.RolloutExporting || ro.Export == failed {
		t.Fatalf("the retry: %+v, failed export %s", ro, failed)
	}
	if h.export(failed).Status.Phase != gentianov1alpha1.TenantExportPhaseFailed {
		t.Fatalf("the failed export was not kept: %+v", h.export(failed).Status)
	}
	if got = h.reconcile(); got.Status.Rollout.Phase != gentianov1alpha1.RolloutExporting {
		t.Fatalf("the retry stopped before its export ended: %+v", got.Status.Rollout)
	}
	h.endExport(ro.Export, gentianov1alpha1.TenantExportPhaseReady)
	if got = h.reconcile(); got.Status.Rollout.Phase != gentianov1alpha1.RolloutRolling {
		t.Fatalf("the retry did not roll out: %+v", got.Status.Rollout)
	}
}

// A build that does not come up within the deadline is rolled back.
func TestAMoveThatDoesNotComeUpInTimeIsRolledBack(t *testing.T) {
	newer := strings.ReplaceAll(wikiWithData, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, _ := startMove(t, wikiWithData, newer)
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

// A release reported ready before the app's workloads have rolled out, or
// with no pod running yet, is the build before: the move waits for the new
// one instead of verifying in no pod and rolling back.
func TestAMoveWaitsForTheRolloutBeforeVerifying(t *testing.T) {
	newer := strings.ReplaceAll(wikiWithVerify, `version: "1.0.0"`, `version: "2.0.0"`)
	h, _, newDigest := startMove(t, wikiWithVerify, newer)
	exec := &recordingExec{}
	h.r.Exec = exec
	ctx := context.Background()
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: h.comp.Namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "wiki"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "wiki"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "wiki", Image: "wiki"}}},
			}}}
	if err := h.c.Create(ctx, deployment); err != nil {
		t.Fatal(err)
	}

	got := h.reconcile()
	h.endExport(got.Status.Rollout.Export, gentianov1alpha1.TenantExportPhaseReady)
	h.reconcile()
	h.markReleaseReady()
	got = h.reconcile()
	if ro := got.Status.Rollout; ro.Phase != gentianov1alpha1.RolloutRolling ||
		!strings.Contains(componentReadyCondition(got).Message, "deployment/wiki") {
		t.Fatalf("while the deployment rolls out: %+v %+v", ro, componentReadyCondition(got))
	}

	// Rolled out, but its pod is not running yet.
	rollOutWorkloads(t, h.c, h.comp.Namespace)
	got = h.reconcile()
	if ro := got.Status.Rollout; ro.Phase != gentianov1alpha1.RolloutRolling ||
		!strings.Contains(componentReadyCondition(got).Message, "running pod") {
		t.Fatalf("with no running pod: %+v %+v", ro, componentReadyCondition(got))
	}
	if len(exec.ran) != 0 {
		t.Fatalf("verified before the build was up: %v", exec.ran)
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "wiki-0", Namespace: h.comp.Namespace,
		Labels: map[string]string{"app.kubernetes.io/name": "wiki"}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "wiki", Image: "wiki"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if err := h.c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	got = h.reconcile()
	if ro := got.Status.Rollout; ro.Phase != gentianov1alpha1.RolloutDone || ro.Digest != newDigest || len(exec.ran) != 1 {
		t.Fatalf("once the pod runs: %+v, verified %v", ro, exec.ran)
	}
}

// A move still waiting when the deadline passes is rolled back, and says on
// what it waited.
func TestAMoveWaitingPastTheDeadlineIsRolledBack(t *testing.T) {
	newer := strings.ReplaceAll(wikiWithVerify, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, _ := startMove(t, wikiWithVerify, newer)
	h.r.Exec = &recordingExec{}
	got := h.reconcile()
	h.endExport(got.Status.Rollout.Export, gentianov1alpha1.TenantExportPhaseReady)
	h.reconcile()
	h.markReleaseReady()
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
	if ro := got.Status.Rollout; ro.Phase != gentianov1alpha1.RolloutRolledBack || ro.Reason != rolloutReasonNotReadyInTime ||
		ro.Digest != oldDigest || !strings.Contains(ro.Message, "running pod") {
		t.Fatalf("after the deadline: %+v", ro)
	}
}

// Once a move has ended, the newest exports taken before the app's moves are
// kept, with the one its last move made, and older finished ones go; another
// app's, a backup somebody took, and an export still running are not touched.
func TestOlderExportsTakenBeforeMovesAreRemoved(t *testing.T) {
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	export := func(name, app, reason string, day int, phase gentianov1alpha1.TenantExportPhase) client.Object {
		labels := map[string]string{componentLabel: app}
		if reason != "" {
			labels[gentianov1alpha1.TenantExportReasonLabel] = reason
		}
		return &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-demo", Labels: labels,
				CreationTimestamp: metav1.Time{Time: time.Date(2026, 11, day, 3, 0, 0, 0, time.UTC)}},
			Status: gentianov1alpha1.TenantExportStatus{Phase: phase},
		}
	}
	ready, running := gentianov1alpha1.TenantExportPhaseReady, gentianov1alpha1.TenantExportPhaseRunning
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		export("move-wiki-1", "wiki", "move", 1, ready),
		export("move-wiki-2", "wiki", "move", 2, gentianov1alpha1.TenantExportPhaseFailed),
		export("move-wiki-3", "wiki", "move", 3, ready),
		export("move-wiki-4", "wiki", "move", 4, ready),
		export("move-wiki-5", "wiki", "move", 5, running),
		export("move-shop-1", "shop", "move", 1, ready),
		export("nightly", "wiki", "", 1, ready),
	).Build()
	r := &ComponentReconciler{Client: c, Scheme: s}
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.ComponentStatus{Rollout: &gentianov1alpha1.RolloutStatus{
			Phase: gentianov1alpha1.RolloutDone, Export: "move-wiki-1"}},
	}
	if err := r.pruneMoveExports(context.Background(), comp); err != nil {
		t.Fatal(err)
	}
	list := &gentianov1alpha1.TenantExportList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range list.Items {
		left = append(left, e.Name)
	}
	got := strings.Join(left, ",")
	if got != "move-shop-1,move-wiki-1,move-wiki-3,move-wiki-4,move-wiki-5,nightly" {
		t.Fatalf("left %s; want the two newest finished, the last move's, the running one, and the others' untouched", got)
	}
}

// backupPolicy is a backup policy saying what is exported before a move: the
// cluster's when tenant is "", else that tenant's own.
func backupPolicy(tenant string, beforeMove gentianov1alpha1.BeforeMoveExport, allowOverride *bool) *gentianov1alpha1.BackupPolicy {
	p := &gentianov1alpha1.BackupPolicy{Spec: gentianov1alpha1.BackupPolicySpec{
		Scope: "cluster", BeforeMove: beforeMove, AllowTenantOverride: allowOverride}}
	p.Name = clusterBackupPolicy
	if tenant != "" {
		p.Name, p.Spec.Scope, p.Spec.Tenant = tenant, "tenant", tenant
	}
	return p
}

// startedWithoutExport checks that the pass after the pin moved rolled the
// pinned build out at once, with no export taken.
func startedWithoutExport(t *testing.T, h *digestHarness, oldDigest, newDigest string) {
	t.Helper()
	got := h.reconcile()
	ro := got.Status.Rollout
	if ro.Phase != gentianov1alpha1.RolloutRolling || ro.Target != newDigest || ro.Digest != oldDigest || ro.Export != "" || ro.StartedAt == nil {
		t.Fatalf("after the pin moved: %+v", ro)
	}
	if h.releasedVersion() != "2.0.0" {
		t.Fatalf("the release is at %q, not the pinned build", h.releasedVersion())
	}
	exports := &gentianov1alpha1.TenantExportList{}
	if err := h.c.List(context.Background(), exports); err != nil {
		t.Fatal(err)
	}
	if len(exports.Items) != 0 {
		t.Fatalf("an export was taken: %s", exports.Items[0].Name)
	}
	// And it ends as any move does.
	h.markReleaseReady()
	ro = h.reconcile().Status.Rollout
	if ro.Phase != gentianov1alpha1.RolloutDone || ro.Digest != newDigest || ro.Previous != oldDigest {
		t.Fatalf("after the release came up: %+v", ro)
	}
}

// An app that holds no data -- no database, no bucket, no volume -- is not
// exported before a move, whatever the policy says: the pinned build rolls
// out at once.
func TestAMoveOfAnAppThatHoldsNothingRollsOutAtOnce(t *testing.T) {
	newer := strings.ReplaceAll(wikiBundle, `version: "1.0.0"`, `version: "2.0.0"`)
	h, oldDigest, newDigest := startMove(t, wikiBundle, newer)
	if err := h.c.Create(context.Background(), backupPolicy("", gentianov1alpha1.BeforeMoveTenant, nil)); err != nil {
		t.Fatal(err)
	}
	startedWithoutExport(t, h, oldDigest, newDigest)
}

// A volume the app's release holds is data, though the profile names none.
func TestAMoveOfAnAppWithAVolumeExportsIt(t *testing.T) {
	newer := strings.ReplaceAll(wikiBundle, `version: "1.0.0"`, `version: "2.0.0"`)
	h, _, _ := startMove(t, wikiBundle, newer)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-wiki-0", Namespace: h.comp.Namespace,
		Labels: map[string]string{"gentianos.io/app": "wiki"}}}
	if err := h.c.Create(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	if ro := h.reconcile().Status.Rollout; ro.Phase != gentianov1alpha1.RolloutExporting || h.export(ro.Export) == nil {
		t.Fatalf("after the pin moved: %+v", ro)
	}
}

// The policy says what a move of an app with data exports: the app and the
// tenant's own, or nothing.
func TestTheBackupPolicySaysWhatAMoveExports(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name     string
		policies []client.Object
		// core is whether the export captures the tenant's own; none that
		// there is no export.
		core, none bool
	}{
		{name: "no policy: the app alone"},
		{name: "the cluster's: tenant", policies: []client.Object{backupPolicy("", gentianov1alpha1.BeforeMoveTenant, nil)}, core: true},
		{name: "the tenant's over the cluster's", policies: []client.Object{
			backupPolicy("", gentianov1alpha1.BeforeMoveTenant, nil), backupPolicy("acme", gentianov1alpha1.BeforeMoveApp, nil)}},
		{name: "the tenant's: none", policies: []client.Object{backupPolicy("acme", gentianov1alpha1.BeforeMoveNone, nil)}, none: true},
		{name: "the tenant's, where the cluster allows none", policies: []client.Object{
			backupPolicy("", gentianov1alpha1.BeforeMoveTenant, &no), backupPolicy("acme", gentianov1alpha1.BeforeMoveNone, nil)}, core: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newer := strings.ReplaceAll(wikiWithData, `version: "1.0.0"`, `version: "2.0.0"`)
			h, oldDigest, newDigest := startMove(t, wikiWithData, newer)
			for _, p := range tc.policies {
				if err := h.c.Create(context.Background(), p); err != nil {
					t.Fatal(err)
				}
			}
			if tc.none {
				startedWithoutExport(t, h, oldDigest, newDigest)
				return
			}
			ro := h.reconcile().Status.Rollout
			export := h.export(ro.Export)
			if ro.Phase != gentianov1alpha1.RolloutExporting || export == nil {
				t.Fatalf("after the pin moved: %+v", ro)
			}
			if export.CapturesTenantCore() != tc.core {
				t.Fatalf("the export captures the tenant's own: %v, want %v", export.CapturesTenantCore(), tc.core)
			}
		})
	}
}
