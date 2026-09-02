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
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func scheduleReconciler(t *testing.T, now time.Time, objs ...client.Object) *TenantExportScheduleReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("gentian scheme: %v", err)
	}
	return &TenantExportScheduleReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).
			WithStatusSubresource(&gentianov1alpha1.TenantExportSchedule{}).
			WithObjects(objs...).Build(),
		Scheme: s,
		Now:    func() time.Time { return now },
	}
}

// scheduleCreated is when every fixture schedule was declared. Stated rather
// than left zero because dueAt anchors a schedule that has never fired on its
// own creation, so a zero timestamp would exercise a path no admitted object
// ever takes.
var scheduleCreated = time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC)

func nightly(name string) *gentianov1alpha1.TenantExportSchedule {
	return &gentianov1alpha1.TenantExportSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "tenant-demo",
			CreationTimestamp: metav1.Time{Time: scheduleCreated},
		},
		Spec: gentianov1alpha1.TenantExportScheduleSpec{Schedule: "0 3 * * *"},
	}
}

func reconcileSchedule(t *testing.T, r *TenantExportScheduleReconciler, name string) *gentianov1alpha1.TenantExportSchedule {
	t.Helper()
	key := types.NamespacedName{Name: name, Namespace: "tenant-demo"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	out := &gentianov1alpha1.TenantExportSchedule{}
	if err := r.Get(context.Background(), key, out); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	return out
}

func exportsIn(t *testing.T, r *TenantExportScheduleReconciler) []gentianov1alpha1.TenantExport {
	t.Helper()
	list := &gentianov1alpha1.TenantExportList{}
	if err := r.List(context.Background(), list, client.InNamespace("tenant-demo")); err != nil {
		t.Fatalf("list exports: %v", err)
	}
	return list.Items
}

// Declaring a schedule must not immediately take a backup: that would pause a
// tenant's apps as a side effect of writing YAML.
func TestNewScheduleDoesNotFireImmediately(t *testing.T) {
	r := scheduleReconciler(t, scheduleCreated, nightly("nightly"))

	out := reconcileSchedule(t, r, "nightly")

	if got := len(exportsIn(t, r)); got != 0 {
		t.Errorf("created %d export(s) on first reconcile, want 0", got)
	}
	if out.Status.NextScheduleTime == nil {
		t.Error("no next schedule time published")
	}
}

// Waiting for the first window must not become waiting for ever. Because
// LastScheduleTime is written only by a firing, anchoring on it alone let a
// schedule that had never run decline every window and republish
// nextScheduleTime a night further out indefinitely — a nightly backup that
// reported itself Ready, never fired, and wrote nothing to its bucket.
func TestFirstWindowFiresAfterTheNewScheduleHasWaited(t *testing.T) {
	firstWindow := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	r := scheduleReconciler(t, scheduleCreated, nightly("nightly"))

	out := reconcileSchedule(t, r, "nightly")
	if got := len(exportsIn(t, r)); got != 0 {
		t.Fatalf("created %d export(s) at declaration time, want 0", got)
	}
	if out.Status.NextScheduleTime == nil || !out.Status.NextScheduleTime.Time.Equal(firstWindow) {
		t.Fatalf("nextScheduleTime = %v, want %v", out.Status.NextScheduleTime, firstWindow)
	}

	// The window the schedule just published for itself arrives.
	r.Now = func() time.Time { return firstWindow }
	out = reconcileSchedule(t, r, "nightly")

	if got := len(exportsIn(t, r)); got != 1 {
		t.Fatalf("created %d export(s) at the first window, want 1", got)
	}
	if out.Status.LastScheduleTime == nil {
		t.Error("lastScheduleTime not recorded; the next window would deadlock the same way")
	}
	next := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	if out.Status.NextScheduleTime == nil || !out.Status.NextScheduleTime.Time.Equal(next) {
		t.Errorf("nextScheduleTime = %v, want %v", out.Status.NextScheduleTime, next)
	}
}

func TestScheduleFiresWhenDue(t *testing.T) {
	schedule := nightly("nightly")
	yesterday := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	schedule.Status.LastScheduleTime = &metav1.Time{Time: yesterday}

	now := time.Date(2026, 8, 18, 3, 0, 30, 0, time.UTC)
	r := scheduleReconciler(t, now, schedule)

	out := reconcileSchedule(t, r, "nightly")

	exports := exportsIn(t, r)
	if len(exports) != 1 {
		t.Fatalf("created %d export(s), want 1", len(exports))
	}
	if exports[0].Labels[scheduleLabel] != "nightly" {
		t.Errorf("export not labelled with its schedule: %v", exports[0].Labels)
	}
	if out.Status.LastExportName != exports[0].Name {
		t.Errorf("lastExportName = %q, want %q", out.Status.LastExportName, exports[0].Name)
	}
}

// Waking to a long outage must not take a burst of backups whose contents are
// identical, each pausing the tenant's apps again.
func TestALongMissedWindowIsSkippedRatherThanCaughtUp(t *testing.T) {
	schedule := nightly("nightly")
	schedule.Status.LastScheduleTime = &metav1.Time{Time: time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)}

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	r := scheduleReconciler(t, now, schedule)

	reconcileSchedule(t, r, "nightly")

	if got := len(exportsIn(t, r)); got != 0 {
		t.Errorf("created %d export(s) for a stale window, want 0", got)
	}
}

func TestSuspendStopsNewExportsButKeepsHistory(t *testing.T) {
	schedule := nightly("nightly")
	schedule.Spec.Suspend = true
	schedule.Status.LastScheduleTime = &metav1.Time{Time: time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)}

	now := time.Date(2026, 8, 18, 3, 0, 30, 0, time.UTC)
	r := scheduleReconciler(t, now, schedule)

	out := reconcileSchedule(t, r, "nightly")

	if got := len(exportsIn(t, r)); got != 0 {
		t.Errorf("suspended schedule created %d export(s)", got)
	}
	if out.Status.LastScheduleTime == nil {
		t.Error("suspending discarded the history")
	}
}

// An export already running holds the tenant; a second would queue behind it
// and pause the same apps twice.
func TestScheduleSkipsWhileAnExportIsStillRunning(t *testing.T) {
	schedule := nightly("nightly")
	schedule.Status.LastScheduleTime = &metav1.Time{Time: time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)}

	running := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-20260817-0300",
			Namespace: "tenant-demo",
			Labels:    map[string]string{scheduleLabel: "nightly"},
		},
		Status: gentianov1alpha1.TenantExportStatus{Phase: gentianov1alpha1.TenantExportPhaseRunning},
	}

	now := time.Date(2026, 8, 18, 3, 0, 30, 0, time.UTC)
	r := scheduleReconciler(t, now, schedule, running)

	reconcileSchedule(t, r, "nightly")

	if got := len(exportsIn(t, r)); got != 1 {
		t.Errorf("created a second export while one was running: %d total", got)
	}
}

func TestInvalidScheduleIsReportedNotRetriedSilently(t *testing.T) {
	schedule := nightly("broken")
	schedule.Spec.Schedule = "not a cron expression"
	r := scheduleReconciler(t, time.Now().UTC(), schedule)

	out := reconcileSchedule(t, r, "broken")

	var ready *metav1.Condition
	for i := range out.Status.Conditions {
		if out.Status.Conditions[i].Type == conditionScheduleReady {
			ready = &out.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("invalid schedule not reported: %+v", out.Status.Conditions)
	}
	if ready.Reason != "InvalidSchedule" {
		t.Errorf("reason = %q", ready.Reason)
	}
}

// Retention deletes finished exports oldest first — and must never touch a
// running one, whose apps would be left paused with nothing to resume them.
func TestRetentionKeepsTheNewestAndSparesRunningExports(t *testing.T) {
	schedule := nightly("nightly")
	schedule.Spec.KeepLast = 2

	objs := []client.Object{schedule}
	for i, day := range []int{10, 11, 12, 13} {
		objs = append(objs, &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "nightly-2026081" + string(rune('0'+i)),
				Namespace:         "tenant-demo",
				Labels:            map[string]string{scheduleLabel: "nightly"},
				CreationTimestamp: metav1.Time{Time: time.Date(2026, 8, day, 3, 0, 0, 0, time.UTC)},
			},
			Status: gentianov1alpha1.TenantExportStatus{Phase: gentianov1alpha1.TenantExportPhaseReady},
		})
	}
	objs = append(objs, &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nightly-running",
			Namespace:         "tenant-demo",
			Labels:            map[string]string{scheduleLabel: "nightly"},
			CreationTimestamp: metav1.Time{Time: time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)},
		},
		Status: gentianov1alpha1.TenantExportStatus{Phase: gentianov1alpha1.TenantExportPhaseRunning},
	})

	r := scheduleReconciler(t, time.Date(2026, 8, 14, 3, 0, 30, 0, time.UTC), objs...)
	reconcileSchedule(t, r, "nightly")

	var finished, running int
	for _, export := range exportsIn(t, r) {
		if export.Status.Phase == gentianov1alpha1.TenantExportPhaseRunning {
			running++
			continue
		}
		if export.Status.Phase == gentianov1alpha1.TenantExportPhaseReady {
			finished++
		}
	}
	if finished != 2 {
		t.Errorf("kept %d finished export(s), want 2", finished)
	}
	if running != 1 {
		t.Error("retention deleted a running export, stranding whatever it had paused")
	}
}

// The tiers reach where keepLast cannot. Thirty nightly exports with
// keepLast: 2 alone would leave two days of history; adding a monthly tier
// keeps a bundle from the older month as well, which is the whole reason the
// tiers exist.
func TestScheduleRetentionTiersKeepOlderHistory(t *testing.T) {
	schedule := nightly("nightly")
	schedule.Spec.Retention = &gentianov1alpha1.BackupRetention{KeepLast: 2, KeepMonthly: 2}

	objs := []client.Object{schedule}
	// One in July, three in August.
	for i, at := range []time.Time{
		time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC),
	} {
		stamp := metav1.Time{Time: at}
		objs = append(objs, &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "nightly-" + string(rune('a'+i)),
				Namespace:         "tenant-demo",
				Labels:            map[string]string{scheduleLabel: "nightly"},
				CreationTimestamp: stamp,
			},
			Status: gentianov1alpha1.TenantExportStatus{
				Phase:       gentianov1alpha1.TenantExportPhaseReady,
				CompletedAt: &stamp,
			},
		})
	}

	r := scheduleReconciler(t, time.Date(2026, 8, 14, 3, 0, 30, 0, time.UTC), objs...)
	reconcileSchedule(t, r, "nightly")

	surviving := map[string]bool{}
	for _, export := range exportsIn(t, r) {
		surviving[export.Name] = true
	}
	if !surviving["nightly-a"] {
		t.Error("the July export was deleted; the monthly tier should have kept it")
	}
	if !surviving["nightly-d"] || !surviving["nightly-c"] {
		t.Error("keepLast did not retain the two most recent")
	}
	if surviving["nightly-b"] {
		t.Error("11 August survived; no rule keeps it once c and d cover August")
	}
}

func TestRetentionOffKeepsEverything(t *testing.T) {
	schedule := nightly("nightly")
	objs := []client.Object{schedule}
	for i := 0; i < 5; i++ {
		objs = append(objs, &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "keep-" + string(rune('a'+i)),
				Namespace:         "tenant-demo",
				Labels:            map[string]string{scheduleLabel: "nightly"},
				CreationTimestamp: metav1.Time{Time: time.Date(2026, 8, 10+i, 3, 0, 0, 0, time.UTC)},
			},
			Status: gentianov1alpha1.TenantExportStatus{Phase: gentianov1alpha1.TenantExportPhaseReady},
		})
	}
	r := scheduleReconciler(t, time.Date(2026, 8, 20, 3, 0, 30, 0, time.UTC), objs...)
	reconcileSchedule(t, r, "nightly")

	if got := len(exportsIn(t, r)); got < 5 {
		t.Errorf("keepLast=0 deleted exports: %d remain", got)
	}
}

func TestQuiesceModeIsRecoveredFromTheStatusMessage(t *testing.T) {
	cases := map[string]gentianov1alpha1.BackupQuiesceMode{
		"paused (command)":   gentianov1alpha1.BackupQuiesceCommand,
		"paused (scaleDown)": gentianov1alpha1.BackupQuiesceScaleDown,
		"paused (none)":      gentianov1alpha1.BackupQuiesceNone,
		"":                   gentianov1alpha1.BackupQuiesceScaleDown,
	}
	for message, want := range cases {
		if got := quiesceModeFromMessage(message); got != want {
			t.Errorf("quiesceModeFromMessage(%q) = %q, want %q", message, got, want)
		}
	}
}

// The failure this covers ran on a real cluster for two nights: a schedule
// declared on 08-31, whose first window at 09-01 03:00 passed while the
// operator was on an older image, then reported Ready and nextScheduleTime one
// night further out every night, for ever, without taking a backup.
//
// The existing first-window test reconciles exactly at the window, where the
// staleness is zero and inside scheduleMissedWindow. It therefore never
// exercised a first evaluation that arrives late, which is the only way this
// fails — and the reason two successive anchor fixes both looked correct.
func TestAMissedFirstWindowIsSkippedOnceAndThenTheNextOneFires(t *testing.T) {
	firstWindow := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	// Nobody reconciles until long after the first window: more than a day, so
	// far outside scheduleMissedWindow that it must be skipped rather than taken.
	late := firstWindow.Add(26 * time.Hour)
	r := scheduleReconciler(t, late, nightly("nightly"))

	out := reconcileSchedule(t, r, "nightly")
	if got := len(exportsIn(t, r)); got != 0 {
		t.Fatalf("created %d export(s) for a window %s stale, want 0 — a backup "+
			"that late is the burst the missed-window rule exists to prevent",
			got, late.Sub(firstWindow))
	}
	// The anchor must have moved to the next window after now — 08-19 03:00 is
	// already behind the late reconcile, so the one it can still keep is 08-20.
	// Leaving it on the missed window is what made every later evaluation skip.
	nextWindow := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	if out.Status.NextScheduleTime == nil || !out.Status.NextScheduleTime.Time.Equal(nextWindow) {
		t.Fatalf("nextScheduleTime = %v, want %v", out.Status.NextScheduleTime, nextWindow)
	}

	// The window it just committed to arrives, and this is the step that was
	// never taken: the schedule must fire rather than skip again.
	r.Now = func() time.Time { return nextWindow }
	out = reconcileSchedule(t, r, "nightly")

	if got := len(exportsIn(t, r)); got != 1 {
		t.Fatalf("created %d export(s) at the window after the missed one, want 1 — "+
			"the schedule is wedged and will never back anything up", got)
	}
	if out.Status.LastScheduleTime == nil {
		t.Error("lastScheduleTime not recorded after firing")
	}
}
