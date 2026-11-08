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
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// runJob is a unit Job of tenant's run in namespace, running unless ended.
func runJob(name, namespace, tenant, run string, ended batchv1.JobConditionType) *batchv1.Job {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
		Labels: map[string]string{backup.ExportLabel: run, tenantLabel: tenant}}}
	if ended != "" {
		job.Status.Conditions = []batchv1.JobCondition{{Type: ended, Status: corev1.ConditionTrue}}
	}
	return job
}

// deletions records what is deleted, in order, and the propagation each Job
// was deleted with.
type deletions struct {
	order       []string
	propagation map[string]metav1.DeletionPropagation
}

func (d *deletions) funcs() interceptor.Funcs {
	d.propagation = map[string]metav1.DeletionPropagation{}
	return interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			kind := "Secret"
			if _, ok := obj.(*batchv1.Job); ok {
				kind = "Job"
				applied := &client.DeleteOptions{}
				applied.ApplyOptions(opts)
				if applied.PropagationPolicy != nil {
					d.propagation[obj.GetName()] = *applied.PropagationPolicy
				}
			}
			err := cl.Delete(ctx, obj, opts...)
			if err == nil {
				d.order = append(d.order, kind+"/"+obj.GetName())
			}
			return err
		},
	}
}

func jobExists(t *testing.T, c client.Client, job *batchv1.Job) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &batchv1.Job{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// The Jobs a run leaves when it ends: its own still running, which have to
// go, and the rest, which stay.
type runJobs struct {
	running, completed, failed, otherTenant, otherRun *batchv1.Job
}

func newRunJobs(run string) runJobs {
	return runJobs{
		running:     runJob("tx-demo-"+run+"-notes-vol0", "tenant-demo", "demo", run, ""),
		completed:   runJob("tx-demo-"+run+"-notes-pg", postgresNamespace, "demo", run, batchv1.JobComplete),
		failed:      runJob("tx-demo-"+run+"-notes-s3", s3Namespace, "demo", run, batchv1.JobFailed),
		otherTenant: runJob("tx-acme-"+run+"-notes-pg", postgresNamespace, "acme", run, ""),
		otherRun:    runJob("tx-demo-another-notes-pg", postgresNamespace, "demo", "another", ""),
	}
}

func (j runJobs) objects() []client.Object {
	return []client.Object{j.running, j.completed, j.failed, j.otherTenant, j.otherRun}
}

// check says that the run's running Job is gone, deleted with its pods before
// any Secret was, and that every other Job is still there.
func (j runJobs) check(t *testing.T, c client.Client, d *deletions) {
	t.Helper()
	if jobExists(t, c, j.running) {
		t.Errorf("the run ended and its running Job %s is still there", j.running.Name)
	}
	if got := d.propagation[j.running.Name]; got != metav1.DeletePropagationBackground {
		t.Errorf("Job deleted with propagation %q, want Background so its pods go", got)
	}
	jobAt := slices.Index(d.order, "Job/"+j.running.Name)
	for i, what := range d.order {
		if strings.HasPrefix(what, "Secret/") && i < jobAt {
			t.Errorf("%s was deleted before the running Job: %v", what, d.order)
		}
	}
	for _, kept := range []*batchv1.Job{j.completed, j.failed, j.otherTenant, j.otherRun} {
		if !jobExists(t, c, kept) {
			t.Errorf("Job %s/%s was deleted; only the run's own running Jobs go", kept.Namespace, kept.Name)
		}
	}
}

func newStopExportWorld(t *testing.T, phase gentianov1alpha1.TenantExportPhase, extra ...client.Object) (*TenantExportReconciler, *gentianov1alpha1.TenantExport, runJobs, *deletions) {
	t.Helper()
	s := quiesceScheme(t)
	started := metav1.Now()
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "export-x", Namespace: "tenant-demo", Finalizers: []string{exportFinalizer}},
		Status:     gentianov1alpha1.TenantExportStatus{Phase: phase, StartedAt: &started},
	}
	jobs := newRunJobs("export-x")
	creds := runSecret(stagedSecretName("demo", "export-x"), "tenant-demo", "demo", "export-x", nil)
	objects := append([]client.Object{export, creds,
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}}, jobs.objects()...)
	d := &deletions{}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objects, extra...)...).
		WithStatusSubresource(export).WithInterceptorFuncs(d.funcs()).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s, Reconciler: &TenantReconciler{Client: c, Scheme: s}}
	return r, export, jobs, d
}

// An export that fails stops its units still running, with their pods,
// before it discards the Secrets they run with: a unit left behind waits on
// them for ever and keeps the app's volume attached.
func TestAFailedExportStopsItsRunningJobs(t *testing.T) {
	r, export, jobs, d := newStopExportWorld(t, gentianov1alpha1.TenantExportPhaseRunning)
	if _, err := r.fail(context.Background(), export, "EncryptionUnavailable", "no recipients"); err != nil {
		t.Fatal(err)
	}
	jobs.check(t, r.Client, d)
	if !slices.Contains(d.order, "Secret/"+stagedSecretName("demo", "export-x")) {
		t.Errorf("the run's credentials were not discarded: %v", d.order)
	}
}

// An export that already ended with a unit still running, as one did before
// it stopped its Jobs or when the operator stopped first, has it stopped on
// its next reconcile.
func TestAnEndedExportStopsJobsItLeftRunning(t *testing.T) {
	r, export, jobs, d := newStopExportWorld(t, gentianov1alpha1.TenantExportPhaseFailed)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(export)}); err != nil {
		t.Fatal(err)
	}
	jobs.check(t, r.Client, d)
}

// While a restore of the same name runs, the Jobs under the name may be its
// own: an export ending then stops none.
func TestAnExportEndingLeavesTheJobsOfARestoreOfTheSameName(t *testing.T) {
	started := metav1.Now()
	restore := &gentianov1alpha1.TenantRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "export-x", Namespace: "tenant-demo"},
		Status:     gentianov1alpha1.TenantRestoreStatus{Phase: gentianov1alpha1.TenantExportPhaseRunning, StartedAt: &started},
	}
	r, export, jobs, _ := newStopExportWorld(t, gentianov1alpha1.TenantExportPhaseRunning, restore)
	if _, err := r.fail(context.Background(), export, "CaptureFailed", "boom"); err != nil {
		t.Fatal(err)
	}
	if !jobExists(t, r.Client, jobs.running) {
		t.Error("a running Job under the name of a running restore was stopped by an export")
	}
}

// A restore that fails stops its units still running in the same way, and a
// restore that ended does so again on its next reconcile.
func TestAFailedRestoreStopsItsRunningJobs(t *testing.T) {
	newWorld := func(t *testing.T) (*restoreWorld, runJobs, *deletions) {
		t.Helper()
		w := newRestoreWorld(t, nil)
		jobs := newRunJobs("r1")
		d := &deletions{}
		// The world's client, wrapped to record what is deleted.
		recorded := interceptor.NewClient(w.c.(client.WithWatch), d.funcs())
		for _, obj := range jobs.objects() {
			if err := recorded.Create(context.Background(), obj); err != nil {
				t.Fatal(err)
			}
			if job := obj.(*batchv1.Job); len(job.Status.Conditions) > 0 {
				if err := recorded.Status().Update(context.Background(), job); err != nil {
					t.Fatal(err)
				}
			}
		}
		w.c = recorded
		w.r.Client = recorded
		return w, jobs, d
	}
	t.Run("on failing", func(t *testing.T) {
		w, jobs, d := newWorld(t)
		if _, err := w.r.fail(context.Background(), w.restore(t), "RestoreFailed", "boom"); err != nil {
			t.Fatal(err)
		}
		jobs.check(t, w.c, d)
	})
	t.Run("once ended", func(t *testing.T) {
		w, jobs, d := newWorld(t)
		ended := w.restore(t)
		ended.Status.Phase = gentianov1alpha1.TenantExportPhaseFailed
		if err := w.c.Status().Update(context.Background(), ended); err != nil {
			t.Fatal(err)
		}
		if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
		jobs.check(t, w.c, d)
	})
}

// kindJob is runJob labeled with the kind of run it belongs to.
func kindJob(name, namespace, run, kind string) *batchv1.Job {
	job := runJob(name, namespace, "demo", run, "")
	job.Labels[backup.RunKindLabel] = kind
	return job
}

// A Job says which kind of run it is: an export and a restore of the same
// name each delete only their own, ending or deleted, while the other runs.
// A Job made before the label, with no kind, is left while a run of the
// other kind with the name is under way.
func TestARunDeletesOnlyTheJobsOfItsOwnKind(t *testing.T) {
	started := metav1.Now()
	ctx := context.Background()
	exportJob := kindJob("tx-demo-same-notes-vol0", "tenant-demo", "same", backup.RunKindExport)
	restoreJob := kindJob("tx-demo-same-notes-pgr", postgresNamespace, "same", backup.RunKindRestore)
	unlabeled := runJob("tx-demo-same-notes-s3", s3Namespace, "demo", "same", "")
	jobs := func() []client.Object {
		return []client.Object{exportJob.DeepCopy(), restoreJob.DeepCopy(), unlabeled.DeepCopy()}
	}
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "same", Namespace: "tenant-demo"},
		Status:     gentianov1alpha1.TenantExportStatus{Phase: gentianov1alpha1.TenantExportPhaseRunning, StartedAt: &started},
	}
	restore := &gentianov1alpha1.TenantRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "same", Namespace: "tenant-demo"},
		Status:     gentianov1alpha1.TenantRestoreStatus{Phase: gentianov1alpha1.TenantExportPhaseRunning, StartedAt: &started},
	}

	for name, tc := range map[string]struct {
		run     func(c client.Client) error
		deleted *batchv1.Job
		kept    []*batchv1.Job
	}{
		"an export deleted while the restore runs": {
			run: func(c client.Client) error {
				return (&TenantExportReconciler{Client: c}).deleteExportJobs(ctx, export)
			},
			deleted: exportJob, kept: []*batchv1.Job{restoreJob, unlabeled},
		},
		"an export failing while the restore runs": {
			run: func(c client.Client) error {
				return (&TenantExportReconciler{Client: c}).stopRunningExportJobs(ctx, export)
			},
			deleted: exportJob, kept: []*batchv1.Job{restoreJob, unlabeled},
		},
		"a restore deleted while the export runs": {
			run: func(c client.Client) error {
				return (&TenantRestoreReconciler{Client: c}).deleteRestoreJobs(ctx, restore)
			},
			deleted: restoreJob, kept: []*batchv1.Job{exportJob, unlabeled},
		},
		"a restore failing while the export runs": {
			run: func(c client.Client) error {
				return (&TenantRestoreReconciler{Client: c}).stopRunningRestoreJobs(ctx, restore)
			},
			deleted: restoreJob, kept: []*batchv1.Job{exportJob, unlabeled},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(quiesceScheme(t)).
				WithObjects(append(jobs(), export.DeepCopy(), restore.DeepCopy())...).Build()
			if err := tc.run(c); err != nil {
				t.Fatal(err)
			}
			if jobExists(t, c, tc.deleted) {
				t.Errorf("the run's own Job %s is still there", tc.deleted.Name)
			}
			for _, kept := range tc.kept {
				if !jobExists(t, c, kept) {
					t.Errorf("Job %s was deleted; it is not the run's own, or may not be", kept.Name)
				}
			}
		})
	}

	// With no run of the other kind under way, a Job with no kind label is
	// taken to be the run's own, as every Job was before the label.
	t.Run("an unlabeled Job once the other run has ended", func(t *testing.T) {
		ended := restore.DeepCopy()
		ended.Status.Phase = gentianov1alpha1.TenantExportPhaseFailed
		c := fake.NewClientBuilder().WithScheme(quiesceScheme(t)).
			WithObjects(append(jobs(), export.DeepCopy(), ended)...).Build()
		if err := (&TenantExportReconciler{Client: c}).deleteExportJobs(ctx, export); err != nil {
			t.Fatal(err)
		}
		if jobExists(t, c, unlabeled) || jobExists(t, c, exportJob) {
			t.Error("the export's Jobs, labeled and not, were not deleted")
		}
		if !jobExists(t, c, restoreJob) {
			t.Error("the restore's labeled Job was deleted by an export")
		}
	})
}

// Every Job an export or a restore makes says which kind of run it is.
func TestRunJobParamsSayTheKindOfRun(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	bundle := &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "same"}
	export := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "same", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{Bundle: bundle}}
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "same", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantRestoreStatus{Bundle: bundle}}
	if got := (&TenantExportReconciler{}).jobParams(tenant, "notes", export, backup.Encryption{}).RunKind; got != backup.RunKindExport {
		t.Errorf("export Jobs say run kind %q", got)
	}
	if got := (&TenantRestoreReconciler{}).jobParams(tenant, "notes", restore).RunKind; got != backup.RunKindRestore {
		t.Errorf("restore Jobs say run kind %q", got)
	}
}
