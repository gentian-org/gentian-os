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
	"sort"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
)

// fakeBundles is the object store as a restore reads it.
type fakeBundles struct {
	manifest *backup.Manifest
	err      error
	keys     []bundlestore.Key
	removed  []string
}

func (f *fakeBundles) Manifest(_ context.Context, _ gentianov1alpha1.BundleRef, key bundlestore.Key) (*backup.Manifest, error) {
	f.keys = append(f.keys, key)
	return f.manifest, f.err
}

func (f *fakeBundles) RemoveImported(_ context.Context, ref gentianov1alpha1.BundleRef) error {
	f.removed = append(f.removed, ref.Bucket+"/"+ref.Prefix)
	return nil
}

type restoreWorld struct {
	c       client.Client
	r       *TenantRestoreReconciler
	bundles *fakeBundles
	key     types.NamespacedName
}

// newRestoreWorld is tenant demo with wiki (a database, a bucket, a volume)
// and drive installed, and an uploaded bundle of tenant "old" that holds
// wiki and notes.
func newRestoreWorld(t *testing.T, mutate func(*gentianov1alpha1.TenantRestore)) *restoreWorld {
	t.Helper()
	scheme := deleteGapsScheme()
	tenant := planTenant("demo", "wiki", "drive")
	restore := &gentianov1alpha1.TenantRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-demo"},
		Spec: gentianov1alpha1.TenantRestoreSpec{
			ConfirmTenant: "demo",
			Bundle:        &gentianov1alpha1.BundleRef{Bucket: bundlestore.ImportBucket, Prefix: "20261007-101500-1a2b3c4d"},
			Decryption: &gentianov1alpha1.RestoreDecryption{
				PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "r1-key", Key: "passphrase"}},
		},
	}
	if mutate != nil {
		mutate(restore)
	}
	objects := []client.Object{
		tenant, restore,
		chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		chartProfile("drive", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "r1-key", Namespace: "tenant-demo"},
			Data: map[string][]byte{"passphrase": []byte("correct horse")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: backup.MinIOAdminSecret, Namespace: s3Namespace},
			Data: map[string][]byte{"endpoint": []byte("http://minio:9000"), "accessKey": []byte("a"), "secretKey": []byte("s")}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "wiki-release-data", Namespace: "tenant-demo",
			Annotations: map[string]string{"meta.helm.sh/release-name": "wiki-release"}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "drive-release-data", Namespace: "tenant-demo",
			Annotations: map[string]string{"meta.helm.sh/release-name": "drive-release"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&gentianov1alpha1.TenantRestore{}).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme}
	bundles := &fakeBundles{manifest: wikiManifest()}
	return &restoreWorld{
		c: c, bundles: bundles,
		key: types.NamespacedName{Name: "r1", Namespace: "tenant-demo"},
		r: &TenantRestoreReconciler{Client: c, Scheme: scheme, Tenant: tr, Bundles: bundles,
			Reconciler: &TenantExportReconciler{Client: c, Scheme: scheme, Reconciler: tr}},
	}
}

func (w *restoreWorld) restore(t *testing.T) *gentianov1alpha1.TenantRestore {
	t.Helper()
	got := &gentianov1alpha1.TenantRestore{}
	if err := w.c.Get(context.Background(), w.key, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func (w *restoreWorld) jobs(t *testing.T) map[string]*batchv1.Job {
	t.Helper()
	list := &batchv1.JobList{}
	if err := w.c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	out := map[string]*batchv1.Job{}
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out
}

// run reconciles until the restore ends, finishing every Job it creates.
func (w *restoreWorld) run(t *testing.T) *gentianov1alpha1.TenantRestore {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		for _, job := range w.jobs(t) {
			if len(job.Status.Conditions) == 0 {
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				if err := w.c.Status().Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := w.restore(t); got.IsTerminal() && i > 0 {
			// One more pass: what a terminal restore still has to do.
			if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
				t.Fatalf("terminal reconcile: %v", err)
			}
			return w.restore(t)
		}
	}
	t.Fatalf("the restore did not end: %+v", w.restore(t).Status)
	return nil
}

// A restore reads the bundle's manifest with the key it was given, restores
// what the manifest holds into this tenant's stores, never touches an app the
// bundle does not hold, names what it did not restore, and removes the
// uploaded bundle once it has run.
func TestARestoreRunsFromTheManifestAndReportsWhatItLeftOut(t *testing.T) {
	w := newRestoreWorld(t, nil)
	got := w.run(t)

	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if len(w.bundles.keys) != 1 || w.bundles.keys[0].Passphrase != "correct horse" {
		t.Errorf("the manifest was read %d time(s) with %+v; want once, with the restore's own key", len(w.bundles.keys), w.bundles.keys)
	}
	if got.Status.BundleSchemaVersion != 2 || got.Status.NameDerivation != gentianov1alpha1.RestoreNamesFromManifest {
		t.Errorf("format %d, derivation %q", got.Status.BundleSchemaVersion, got.Status.NameDerivation)
	}

	// notes is in the bundle and not installed: named, and the result is not
	// complete.
	if got.Status.Complete == nil || *got.Status.Complete {
		t.Errorf("complete = %v, want false", got.Status.Complete)
	}
	if len(got.Status.NotRestored) != 1 || got.Status.NotRestored[0].App != "notes" ||
		!strings.Contains(got.Status.NotRestored[0].Reason, "not installed") {
		t.Errorf("notRestored = %+v", got.Status.NotRestored)
	}
	var complete *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == conditionExportComplete {
			complete = &got.Status.Conditions[i]
		}
	}
	if complete == nil || complete.Reason != "PartiallyRestored" || !strings.Contains(complete.Message, "notes is not installed") {
		t.Errorf("Complete condition = %+v", complete)
	}
	// What no restore brings back is said on the result.
	notes := strings.Join(got.Status.Notes, "\n")
	for _, want := range []string{"Stored credentials are not in a bundle", "have to be entered again", "recovery kit"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the result's notes lack %q", want)
		}
	}

	// The Jobs: wiki's four artefacts and the tenant's two; nothing for
	// drive, which the bundle does not hold, and nothing for notes.
	jobs := w.jobs(t)
	var names []string
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.Contains(name, "drive") || strings.Contains(name, "notes") {
			t.Errorf("a Job was run for an app this restore must not touch: %s", name)
		}
	}
	for suffix, wantFetch := range map[string]string{
		"wiki-pgr":              "postgres/old_wiki.pgc",
		"wiki-pgor":             "postgres/old_wiki.owned.tar.gz",
		"wiki-s3r":              "s3/old-wiki.tar.gz",
		"wiki-vr0":              "volumes/wiki-release-data.tar.gz",
		"gentian-tenant-realmr": "identity/realm.tar.gz",
		"gentian-tenant-shellr": "postgres/old_shell.pgc",
	} {
		job := jobs["tx-demo-r1-"+suffix]
		if job == nil {
			t.Errorf("no Job %s among %v", suffix, names)
			continue
		}
		fetch := job.Spec.Template.Spec.InitContainers[0].Args[0]
		if !strings.Contains(fetch, wantFetch) {
			t.Errorf("%s does not fetch %s", suffix, wantFetch)
		}
	}
	// Into this tenant's stores, not the ones the bundle was taken of.
	if pg := jobs["tx-demo-r1-wiki-pgr"]; pg != nil {
		script := pg.Spec.Template.Spec.Containers[0].Args[0]
		if !strings.Contains(script, "DB='demo_wiki'") || strings.Contains(script, "old_wiki") {
			t.Errorf("the database is not restored into demo_wiki:\n%s", script)
		}
	}
	// The bundle is of tenant "old": what is named for a tenant comes back
	// under this one's names. The databases wiki made for itself are told
	// from the old provisioned name and put under the new; the realm's
	// groups are renamed from the old tenant's to this one's; and the
	// result says so.
	if owned := jobs["tx-demo-r1-wiki-pgor"]; owned != nil {
		script := owned.Spec.Template.Spec.Containers[0].Args[0]
		if !strings.Contains(script, "SRC='old_wiki'") || !strings.Contains(script, "DB='demo_wiki'") {
			t.Errorf("the owned databases are not renamed from old_wiki to demo_wiki:\n%s", script)
		}
	}
	if realm := jobs["tx-demo-r1-gentian-tenant-realmr"]; realm != nil {
		script := realm.Spec.Template.Spec.Containers[0].Args[0]
		if !strings.Contains(script, "SRC='old'") || !strings.Contains(script, "DST='demo'") || !strings.Contains(script, "REALM='demo'") {
			t.Errorf("the realm import is not told it renames from old to demo:\n%s", script[:400])
		}
		if realm.Namespace != identityNamespace {
			t.Errorf("the realm import runs in %s", realm.Namespace)
		}
	}
	if got.Status.SourceTenant != "old" || !strings.Contains(notes, "The bundle is of tenant old, not of this one") {
		t.Errorf("sourceTenant = %q; the notes do not say the bundle is another tenant's", got.Status.SourceTenant)
	}

	// The bucket's user and policy are made before its objects are written.
	if s3 := jobs["tx-demo-r1-wiki-s3r"]; s3 != nil {
		inits := s3.Spec.Template.Spec.InitContainers
		if last := inits[len(inits)-1]; last.Name != "provision-bucket" || !strings.Contains(last.Command[2], `mc mb --ignore-existing "gentian/demo-wiki"`) {
			t.Errorf("the bucket is not provisioned before it is filled: %+v", last.Name)
		}
	}
	if vol := jobs["tx-demo-r1-wiki-vr0"]; vol != nil && vol.Namespace != "tenant-demo" {
		t.Errorf("the volume Job runs in %s", vol.Namespace)
	}

	// The upload is removed now that the restore has run, once.
	if len(w.bundles.removed) != 1 || w.bundles.removed[0] != "gentian-imports/20261007-101500-1a2b3c4d" || !got.Status.ImportRemoved {
		t.Errorf("removed = %v, importRemoved = %v", w.bundles.removed, got.Status.ImportRemoved)
	}
	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	if len(w.bundles.removed) != 1 {
		t.Errorf("the upload was removed again: %v", w.bundles.removed)
	}
}

// A restore that cannot read the manifest does not run on a guess, and a
// restore refused before it changed anything keeps the upload.
func TestARestoreThatCannotReadTheManifestChangesNothing(t *testing.T) {
	for name, breakIt := range map[string]func(*restoreWorld){
		"the manifest cannot be opened": func(w *restoreWorld) {
			w.bundles.err = errors.New("the manifest could not be opened with that key")
		},
		"no reader": func(w *restoreWorld) { w.r.Bundles = nil },
	} {
		t.Run(name, func(t *testing.T) {
			w := newRestoreWorld(t, nil)
			removed := w.bundles
			breakIt(w)
			for i := 0; i < 3; i++ {
				if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: w.key}); err != nil {
					t.Fatal(err)
				}
			}
			got := w.restore(t)
			if got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed || got.Status.StartedAt != nil {
				t.Fatalf("phase = %s, startedAt = %v", got.Status.Phase, got.Status.StartedAt)
			}
			if len(w.jobs(t)) != 0 {
				t.Errorf("Jobs were created: %v", w.jobs(t))
			}
			if len(removed.removed) != 0 || got.Status.ImportRemoved {
				t.Errorf("an upload nothing was restored from was removed: %v", removed.removed)
			}
		})
	}
}

// Everything restorable restored: complete, and said so. A tenant's own
// backup is never removed by a restore of it.
func TestACompleteRestoreSaysSoAndKeepsATenantsOwnBundle(t *testing.T) {
	w := newRestoreWorld(t, func(r *gentianov1alpha1.TenantRestore) {
		r.Spec.Bundle = &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"}
		r.Spec.Apps = []string{"wiki"}
	})
	got := w.run(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady || got.Status.Complete == nil || !*got.Status.Complete {
		t.Fatalf("phase = %s, complete = %v, notRestored = %+v", got.Status.Phase, got.Status.Complete, got.Status.NotRestored)
	}
	if len(w.bundles.removed) != 0 || got.Status.ImportRemoved {
		t.Errorf("a tenant's own bundle was removed: %v", w.bundles.removed)
	}
}

// An app named in spec.apps that cannot be restored refuses the restore
// before anything is changed.
func TestARestoreAskedForAnAppItCannotRestoreIsRefused(t *testing.T) {
	w := newRestoreWorld(t, func(r *gentianov1alpha1.TenantRestore) { r.Spec.Apps = []string{"wiki", "notes"} })
	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	got := w.restore(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed || len(w.jobs(t)) != 0 {
		t.Fatalf("phase = %s, jobs = %d", got.Status.Phase, len(w.jobs(t)))
	}
	said := ""
	for _, c := range got.Status.Conditions {
		said += c.Reason + ": " + c.Message + "\n"
	}
	if !strings.Contains(said, "CannotRestoreAsAsked") || !strings.Contains(said, "notes is not installed") {
		t.Errorf("conditions = %s", said)
	}
}

// A restore that began and then failed is not complete, in the field a
// reader looks at; and the upload it read is removed, because it ran.
func TestARestoreThatFailsAfterItBeganSaysItIsNotComplete(t *testing.T) {
	w := newRestoreWorld(t, nil)
	ctx := context.Background()
	// The plan is made and the restore begins.
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	began := w.restore(t)
	if began.Status.StartedAt == nil {
		t.Fatal("the restore did not begin")
	}
	if _, err := w.r.fail(ctx, began, "RestoreFailed", "wiki: restore did not succeed after 3 attempts"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	got := w.restore(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed || got.Status.Complete == nil || *got.Status.Complete {
		t.Fatalf("phase = %s, complete = %v", got.Status.Phase, got.Status.Complete)
	}
	if len(w.bundles.removed) != 1 || !got.Status.ImportRemoved {
		t.Errorf("the upload of a restore that ran and failed was kept: %v", w.bundles.removed)
	}
}
