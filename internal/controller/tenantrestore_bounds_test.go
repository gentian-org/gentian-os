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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// pausedWiki starts a restore in the world and runs it until wiki is paused
// and its Jobs exist.
func pausedWiki(t *testing.T, w *restoreWorld) {
	t.Helper()
	ctx := context.Background()
	one := int32(1)
	if err := w.c.Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-demo", Labels: map[string]string{"gentianos.io/app": "wiki"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if got := wikiReplicas(t, w.c); got != 0 {
		t.Fatalf("wiki has %d replica(s) while it is restored, want it paused", got)
	}
	if len(w.jobs(t)) == 0 {
		t.Fatal("no restore Job was created")
	}
}

func wikiReplicas(t *testing.T, c client.Client) int32 {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "wiki", Namespace: "tenant-demo"}, d); err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func stagedLeft(t *testing.T, c client.Client) []string {
	t.Helper()
	list := &corev1.SecretList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, s := range list.Items {
		if s.Labels[backup.ExportLabel] != "" {
			left = append(left, s.Namespace+"/"+s.Name)
		}
	}
	return left
}

// A unit whose pod cannot be created never fails: nothing happens. A restore
// used to wait on it for ever with the app stopped -- which, while no unit
// could find its Secret, was what every restore of an app with a database
// did. Now it is counted as a capture's is, the restore gives up, starts the
// app again and says what state the data is in.
func TestARestoreWhoseUnitCannotStartGivesUpAndResumesTheApp(t *testing.T) {
	w := newRestoreWorld(t, nil)
	pausedWiki(t, w)
	ctx := context.Background()

	// The database unit's pod never starts: what a Secret that is not there
	// looks like.
	const pg = "tx-demo-r1-wiki-pgr"
	stuck := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: pg + "-abc", Namespace: postgresNamespace,
		Labels:            map[string]string{"job-name": pg},
		CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * capturePodStartDeadline)),
	}}
	stuck.Status.Phase = corev1.PodPending
	stuck.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "fetch", State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `secret "postgres-admin" not found`},
	}}}
	if err := w.c.Create(ctx, stuck); err != nil {
		t.Fatal(err)
	}

	var got *gentianov1alpha1.TenantRestore
	for i := 0; i < 30; i++ {
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if got = w.restore(t); got.IsTerminal() {
			break
		}
	}
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed {
		t.Fatalf("the restore is %s after 30 passes on a unit that cannot start: it waits for ever", got.Status.Phase)
	}
	if got := wikiReplicas(t, w.c); got != 1 {
		t.Errorf("wiki has %d replica(s) after the restore gave up, want it running", got)
	}
	if len(got.Status.Quiesced) != 0 {
		t.Errorf("still recorded as paused: %v", got.Status.Quiesced)
	}
	if got.Status.Complete == nil || *got.Status.Complete {
		t.Errorf("complete = %v", got.Status.Complete)
	}
	said := ""
	for _, c := range got.Status.Conditions {
		said += c.Message + "\n"
	}
	for _, want := range []string{
		"did not start", `secret "postgres-admin" not found`,
		"Every app this restore paused is running again",
		"The data of wiki may be part restored",
		"Not touched: the realm and the desktop's database",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the result does not say %q:\n%s", want, said)
		}
	}
	// One more pass, as the cluster makes: nothing staged outlives the run,
	// and the finalizer goes, since nothing is left to put back.
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	if left := stagedLeft(t, w.c); len(left) != 0 {
		t.Errorf("staged Secrets outlived the failed restore: %v", left)
	}
	if fin := w.restore(t).Finalizers; len(fin) != 0 {
		t.Errorf("a finished restore still holds %v", fin)
	}
}

// Deleting a restore is the only way to stop one. It used to remove the
// object and leave the app it was at paused, with its Jobs still loading.
func TestDeletingARunningRestoreResumesTheAppAndStopsItsJobs(t *testing.T) {
	w := newRestoreWorld(t, nil)
	pausedWiki(t, w)
	ctx := context.Background()
	if len(stagedLeft(t, w.c)) == 0 {
		t.Fatal("nothing was staged for the units that run beside the database and in the tenant's namespace")
	}
	// A live namespace: this is a person deleting the restore, not the tenant
	// going.
	if err := w.c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-demo"}}); err != nil {
		t.Fatal(err)
	}

	if err := w.c.Delete(ctx, w.restore(t)); err != nil {
		t.Fatal(err)
	}
	if still := w.restore(t); still.DeletionTimestamp.IsZero() {
		t.Fatal("the restore holds no finalizer: deleting it removes it before anything is put back")
	}
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}

	if got := wikiReplicas(t, w.c); got != 1 {
		t.Errorf("wiki has %d replica(s) after its restore was deleted, want it running", got)
	}
	if jobs := w.jobs(t); len(jobs) != 0 {
		t.Errorf("%d Job(s) of the deleted restore still run", len(jobs))
	}
	if left := stagedLeft(t, w.c); len(left) != 0 {
		t.Errorf("staged Secrets outlived the deleted restore: %v", left)
	}
	if err := w.c.Get(ctx, w.key, &gentianov1alpha1.TenantRestore{}); !apierrors.IsNotFound(err) {
		t.Errorf("the restore is still there: %v", err)
	}
}

// A restore that goes because its tenant does is not held, even when what it
// paused cannot be resumed: the workloads are going too, and holding it would
// hold the namespace. A restore of a tenant that stays is held until the app
// runs again.
func TestARestoreIsNotHeldWhenItsTenantIsGoing(t *testing.T) {
	w := newRestoreWorld(t, nil)
	pausedWiki(t, w)
	ctx := context.Background()
	// The tenant is gone, and its namespace with it.
	tenant := &gentianov1alpha1.Tenant{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "demo"}, tenant); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Delete(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Delete(ctx, w.restore(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Get(ctx, w.key, &gentianov1alpha1.TenantRestore{}); !apierrors.IsNotFound(err) {
		t.Errorf("the restore of a tenant that is going is still held: %v", err)
	}
}

// A unit that finished is on record and is not run again when its Job has
// been collected: a restore Job loads data, and running it twice replaces
// what the app has written since.
func TestARestoreUnitThatFinishedIsNotRunAgain(t *testing.T) {
	w := newRestoreWorld(t, nil)
	pausedWiki(t, w)
	ctx := context.Background()
	const pg = "tx-demo-r1-wiki-pgr"
	job := w.jobs(t)[pg]
	if job == nil {
		t.Fatalf("no Job %s", pg)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := w.c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Delete(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	if again := w.jobs(t)[pg]; again != nil {
		t.Error("a finished unit was run again after its Job was collected")
	}
}

// The last stage of an export -- the realm, the desktop's database, the
// manifest -- had no bound: a realm that could not be exported was retried
// for ever and the export stayed Running.
func TestTheTenantWideCaptureGivesUp(t *testing.T) {
	s := quiesceScheme(t)
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "export-x", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{
			Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-backup", Prefix: "export-x"},
		},
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	minio := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: backup.MinIOAdminSecret, Namespace: s3Namespace},
		Data: map[string][]byte{"endpoint": []byte("http://minio:9000"), "accessKey": []byte("a"), "secretKey": []byte("s")}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(export, tenant, minio).WithStatusSubresource(export).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s, Reconciler: &TenantReconciler{Client: c, Scheme: s}}
	ctx := context.Background()

	why := ""
	for i := 0; i < 20 && why == ""; i++ {
		done, err := r.captureTenantWide(ctx, export, tenant, backup.Encryption{})
		if err != nil {
			t.Fatal(err)
		}
		if done {
			t.Fatal("reported done with every Job failing")
		}
		jobs := &batchv1.JobList{}
		if err := c.List(ctx, jobs); err != nil {
			t.Fatal(err)
		}
		for j := range jobs.Items {
			jobs.Items[j].Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			if err := c.Status().Update(ctx, &jobs.Items[j]); err != nil {
				t.Fatal(err)
			}
		}
		why = tenantWideExhausted(export)
	}
	if why == "" {
		t.Fatal("20 passes of failing Jobs and the tenant-wide capture is still retried")
	}
	if !strings.Contains(why, "did not succeed after") {
		t.Errorf("the reason is %q", why)
	}
	if entry := appStatus(&export.Status.Apps, backupTenantComponent); entry.Phase != gentianov1alpha1.TenantExportPhaseFailed {
		t.Errorf("the entry is %s", entry.Phase)
	}
}
