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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// What a backup did not hold and a deletion did not take: the data of
// uninstalled apps, the desktop's database on the kernel's PostgreSQL, the
// mailboxes, and the rights that follow from nothing else. Each in every
// direction it has.

// fakeRights is the rights store.
type fakeRights struct {
	tuples map[authz.Tuple]bool
	writes int
}

func newFakeRights(tuples ...authz.Tuple) *fakeRights {
	f := &fakeRights{tuples: map[authz.Tuple]bool{}}
	for _, t := range tuples {
		f.tuples[t] = true
	}
	return f
}

func (f *fakeRights) Read(_ context.Context, filter authz.Tuple) ([]authz.Tuple, error) {
	var out []authz.Tuple
	for t := range f.tuples {
		if filter.User != "" && t.User != filter.User {
			continue
		}
		if filter.Relation != "" && t.Relation != filter.Relation {
			continue
		}
		// An object filter is the object, or a bare type ("app:").
		bareType := strings.HasSuffix(filter.Object, ":") && strings.HasPrefix(t.Object, filter.Object)
		if filter.Object != "" && t.Object != filter.Object && !bareType {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return rightsText(out[i]) < rightsText(out[j]) })
	return out, nil
}

func (f *fakeRights) Write(_ context.Context, writes, deletes []authz.Tuple) error {
	f.writes++
	for _, t := range deletes {
		delete(f.tuples, t)
	}
	for _, t := range writes {
		f.tuples[t] = true
	}
	return nil
}

func (f *fakeRights) has(user, relation, object string) bool {
	return f.tuples[authz.Tuple{User: user, Relation: relation, Object: object}]
}

// projected is what the operator's projection writes for a tenant with the
// apps given: the entries a backup does not carry.
func projected(cluster, tenant string, apps ...string) []authz.Tuple {
	out := []authz.Tuple{
		{User: "cluster:" + cluster, Relation: "cluster", Object: "tenant:" + tenant},
		{User: "cluster:" + cluster, Relation: "operated_by", Object: "tenant:" + tenant},
		{User: "group:gentian/tenant/" + tenant + "/admins#member", Relation: "admin", Object: "tenant:" + tenant},
		{User: "group:gentian/tenant/" + tenant + "/members#member", Relation: "member", Object: "tenant:" + tenant},
		{User: "group:gentian/tenant/" + tenant + "/perimeter#member", Relation: "perimeter_approver", Object: "tenant:" + tenant},
	}
	for _, app := range apps {
		out = append(out,
			authz.Tuple{User: "tenant:" + tenant, Relation: "tenant", Object: "app:" + tenant + "/" + app},
			authz.Tuple{User: "group:gentian/tenant/" + tenant + "/app/" + app + "#member", Relation: "entitled", Object: "app:" + tenant + "/" + app})
	}
	return out
}

// mailCluster is what a cluster that runs its own mail server has for the
// mailbox units to find: the mail namespace, the server's volume, and the
// server's pod holding it on a node.
func mailCluster() []client.Object {
	return []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mailNamespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "dovecot-dev-mail", Namespace: mailNamespace}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "dovecot-dev-0", Namespace: mailNamespace},
			Spec: corev1.PodSpec{NodeName: "node-7", Volumes: []corev1.Volume{{Name: "mail", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "dovecot-dev-mail"}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
}

func retainedClaim(name, release string) *corev1.PersistentVolumeClaim {
	class := "fast"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-demo",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm", "kubernetes.io/unrelated": "x"},
			Annotations: map[string]string{"meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": "tenant-demo",
				"volume.kubernetes.io/selected-node": "node-1"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("3Gi")}},
		},
	}
}

func databaseRecord(tenant, app string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgDatabaseKind})
	obj.SetName(databaseCRName(tenant, app))
	obj.SetNamespace(postgresNamespace)
	obj.SetLabels(map[string]string{tenantLabel: tenant, managedByLabel: managedByValue, appLabel: app})
	_ = unstructured.SetNestedField(obj.Object, true, "status", "applied")
	return obj
}

// gapsWorld is tenant demo with wiki installed and three apps uninstalled
// with their data kept: notes (a MariaDB database), crm (a bucket) and
// files (a volume claim and a PostgreSQL database the cluster keeps a
// record of). pad was purged: nothing of it is left.
type gapsWorld struct {
	c      client.Client
	tenant *gentianov1alpha1.Tenant
	export *gentianov1alpha1.TenantExport
	er     *TenantExportReconciler
	tr     *TenantReconciler
	rights *fakeRights
}

func newGapsWorld(t *testing.T, extra ...client.Object) *gapsWorld {
	t.Helper()
	scheme := deleteGapsScheme()
	tenant := planTenant("demo", "wiki")
	record := backup.NewProvisionedRecord("demo")
	for app, made := range map[string]backup.Provisioned{
		"wiki":              {DatabaseEngine: gentianov1alpha1.DatabaseEnginePostgreSQL, Database: "demo_wiki", DatabaseUser: "demo_wiki", Bucket: "demo-wiki"},
		"notes":             {DatabaseEngine: gentianov1alpha1.DatabaseEngineMariaDB, Database: "demo_notes", DatabaseUser: "demo_notes"},
		"crm":               {Bucket: "demo-crm", CacheUser: "demo-crm"},
		backup.DesktopStore: {DatabaseEngine: gentianov1alpha1.DatabaseEnginePostgreSQL, Database: "demo_shell"},
	} {
		if _, err := backup.RecordProvisioned(record, app, made); err != nil {
			t.Fatal(err)
		}
	}
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{
			Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"},
		},
	}
	objects := append([]client.Object{
		tenant, export, record,
		chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		chartProfile("notes", "1.0.0", gentianov1alpha1.DatabaseEngineMariaDB, false),
		chartProfile("files", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false),
		chartProfile("pad", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		retainedClaim("wiki-release-data", "wiki-release"),
		retainedClaim("files-release-data", "files-release"),
		databaseRecord("demo", "files"), databaseRecord("demo", "wiki"), databaseRecord("demo", backup.DesktopStore),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: backup.MinIOAdminSecret, Namespace: s3Namespace},
			Data: map[string][]byte{"endpoint": []byte("http://minio:9000"), "accessKey": []byte("a"), "secretKey": []byte("s")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: s3Namespace},
			Data: map[string][]byte{"passphrase": []byte("correct horse")}},
	}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&gentianov1alpha1.TenantRestore{}, &gentianov1alpha1.TenantExport{}).Build()
	rights := newFakeRights(projected("c1", "demo", "wiki")...)
	tr := &TenantReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", Rights: rights, ClusterID: "c1"}
	return &gapsWorld{c: c, tenant: tenant, export: export, tr: tr, rights: rights,
		er: &TenantExportReconciler{Client: c, Scheme: scheme, Reconciler: tr}}
}

var gapsEncryption = backup.Encryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, PassphraseSecret: "p", PassphraseKey: "passphrase"}

// --- gap 1: the data of uninstalled apps ------------------------------------

// An export captures what uninstalled apps left: the apps the tenant still
// holds data for, each by the units an installed app is captured by, and
// marked as retained in the manifest. An app that was purged holds nothing
// and is in no bundle.
func TestAnExportCapturesTheDataOfUninstalledApps(t *testing.T) {
	w := newGapsWorld(t)
	ctx := context.Background()

	set, names, err := w.er.retainedSet(ctx, w.tenant, w.export)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"crm", "files", "notes"}) {
		t.Fatalf("retained apps = %v, want crm, files and notes: not the installed wiki, not the desktop's store, not the purged pad", names)
	}

	want := map[string][]gentianov1alpha1.BundleArtefact{
		"notes": {
			{Kind: bundle.ArtefactMariaDB, Name: "demo_notes", Path: "mariadb/demo_notes.sql.gz"},
			{Kind: bundle.ArtefactMariaDBOwned, Name: "demo_notes", Path: "mariadb/demo_notes.owned.tar.gz"},
		},
		"crm": {{Kind: bundle.ArtefactS3, Name: "demo-crm", Path: "s3/demo-crm.tar.gz"}},
		"files": {
			{Kind: bundle.ArtefactPostgres, Name: "demo_files", Path: "postgres/demo_files.pgc"},
			{Kind: bundle.ArtefactPostgresOwned, Name: "demo_files", Path: "postgres/demo_files.owned.tar.gz"},
			{Kind: bundle.ArtefactVolume, Name: "files-release-data", Path: "volumes/files-release-data.tar.gz", Release: "files-release",
				Claim: &gentianov1alpha1.BundleClaim{Size: "3Gi", AccessModes: []string{"ReadWriteOnce"}, StorageClass: "fast",
					Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
					Annotations: map[string]string{"meta.helm.sh/release-name": "files-release", "meta.helm.sh/release-namespace": "tenant-demo"}}},
		},
	}
	for _, name := range names {
		app := set[name]
		units, err := w.er.unitsFor(ctx, w.tenant, name, app.profile, app.stores, app.claims, true, w.export, gapsEncryption)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := unitArtefacts(units); !reflect.DeepEqual(got, want[name]) {
			t.Errorf("%s: captured\n%+v\nwant\n%+v", name, got, want[name])
		}
	}

	// The same units an installed app is captured by: wiki's volume is
	// captured by the Job files' is, to the same kind of path.
	installed, err := w.er.captureUnits(ctx, w.tenant, "wiki", chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true), w.export, gapsEncryption)
	if err != nil {
		t.Fatal(err)
	}
	kinds := unitKinds(installed)
	if !reflect.DeepEqual(kinds, []string{bundle.ArtefactPostgres, bundle.ArtefactPostgresOwned, bundle.ArtefactS3, bundle.ArtefactVolume}) {
		t.Errorf("an installed app's units = %v", kinds)
	}
	for _, u := range installed {
		if u.Claim != nil {
			t.Errorf("an installed app's volume records its claim: the claim is the chart's to make")
		}
	}

	// One pass of the export for a retained app: nothing is paused, the
	// units' Jobs are made, and the entry says it is a retained app's.
	if _, err := w.er.captureRetained(ctx, w.export, w.tenant, set["notes"], gapsEncryption); err != nil {
		t.Fatal(err)
	}
	entry := appStatus(&w.export.Status.Apps, "notes")
	if !entry.Retained || entry.QuiesceStart != nil || len(w.export.Status.Quiesced) != 0 {
		t.Errorf("entry = %+v, quiesced = %v", entry, w.export.Status.Quiesced)
	}
	jobs := &batchv1.JobList{}
	if err := w.c.List(ctx, jobs, client.InNamespace(mariadbNamespace)); err != nil || len(jobs.Items) != 2 {
		t.Fatalf("the retained app's database units: %d Job(s) in %s, %v", len(jobs.Items), mariadbNamespace, err)
	}

	// The manifest: retained, with no build -- none was running.
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{
		{Name: "wiki", ChartVersion: "2.0.0", Artefacts: unitArtefacts(installed)},
		{Name: "files", Retained: true, Artefacts: want["files"]},
	}
	m := w.er.buildManifest(w.export, w.tenant)
	if m.SchemaVersion != 3 || len(m.Apps) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Apps[0].Retained || !m.Apps[1].Retained || m.Apps[1].ChartVersion != "" || m.Apps[1].DatabaseEngine != "postgresql" {
		t.Errorf("apps = %+v", m.Apps)
	}
	if claim := m.Apps[1].Stores[2].Claim; claim == nil || claim.Size != "3Gi" || claim.Annotations["meta.helm.sh/release-name"] != "files-release" {
		t.Errorf("the manifest does not record the retained app's claim: %+v", m.Apps[1].Stores[2])
	}
	if _, kept := m.Apps[1].Stores[2].Claim.Annotations["volume.kubernetes.io/selected-node"]; kept {
		t.Error("the manifest records what a scheduler wrote on the claim")
	}
}

// An export says what it does not hold, and no longer says it of the data
// uninstalled apps left: that is in the bundle. What it was not asked for is
// named, installed or not.
func TestAnExportSaysWhatItDoesNotHold(t *testing.T) {
	w := newGapsWorld(t)
	ctx := context.Background()

	got, err := w.er.notIncluded(ctx, w.tenant, w.export)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an export of everything says it does not hold: %v", got)
	}

	w.tenant.Spec.Apps = append(w.tenant.Spec.Apps, gentianov1alpha1.TenantApp{Profile: "drive"})
	w.export.Spec.Apps = []string{"wiki", "notes"}
	_, names, err := w.er.retainedSet(ctx, w.tenant, w.export)
	if err != nil || !reflect.DeepEqual(names, []string{"notes"}) {
		t.Fatalf("an export asked for wiki and notes captures the retained %v, %v", names, err)
	}
	got, err = w.er.notIncluded(ctx, w.tenant, w.export)
	if err != nil {
		t.Fatal(err)
	}
	said := strings.Join(got, "\n")
	if !strings.Contains(said, "not asked for (drive)") {
		t.Errorf("the installed app the export was not asked for is not named:\n%s", said)
	}
	if !strings.Contains(said, "uninstalled apps this export was not asked for (crm, files)") {
		t.Errorf("the uninstalled apps the export was not asked for are not named:\n%s", said)
	}
	if strings.Contains(said, backup.DesktopStore) || strings.Contains(said, "notes") {
		t.Errorf("something captured is named as left out:\n%s", said)
	}

	// A restore repeats what its bundle says it does not hold.
	restore := &gentianov1alpha1.TenantRestore{}
	recordPlan(restore, &restorePlan{notIncluded: got})
	if notes := strings.Join(restore.Status.Notes, "\n"); !strings.Contains(notes, "The bundle says it does not hold the data of uninstalled apps this export was not asked for (crm, files)") {
		t.Errorf("the restore's notes do not repeat it:\n%s", notes)
	}
}

func retainedManifest() *backup.Manifest {
	return &backup.Manifest{
		SchemaVersion: 3,
		Tenant:        "demo",
		Identity:      &backup.ManifestIdentity{Realm: "demo", Path: backup.IdentityArtefact},
		Apps: []backup.ManifestApp{
			{Name: "wiki", ChartVersion: "2.0.0", DatabaseEngine: "postgresql", Stores: []backup.ManifestStore{
				{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"},
			}},
			{Name: "notes", Retained: true, DatabaseEngine: "mariadb", Stores: []backup.ManifestStore{
				{Kind: bundle.ArtefactMariaDB, Name: "demo_notes", Path: "mariadb/demo_notes.sql.gz"},
				{Kind: bundle.ArtefactMariaDBOwned, Name: "demo_notes", Path: "mariadb/demo_notes.owned.tar.gz"},
			}},
			{Name: "files", Retained: true, DatabaseEngine: "postgresql", Stores: []backup.ManifestStore{
				{Kind: bundle.ArtefactPostgres, Name: "demo_files", Path: "postgres/demo_files.pgc"},
				{Kind: bundle.ArtefactVolume, Name: "files-release-data", Path: "volumes/files-release-data.tar.gz", Release: "files-release",
					Claim: &bundle.ManifestClaim{Size: "3Gi", AccessModes: []string{"ReadWriteOnce"}, StorageClass: "fast",
						Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
						Annotations: map[string]string{"meta.helm.sh/release-name": "files-release", "meta.helm.sh/release-namespace": "tenant-demo"}}},
			}},
			{Name: "crm", Retained: true, Stores: []backup.ManifestStore{
				{Kind: bundle.ArtefactS3, Name: "demo-crm", Path: "s3/demo-crm.tar.gz"},
			}},
		},
	}
}

// What a restore does with an app the bundle holds as retained is decided
// by what the tenant holds of it now: put back into the stores it still
// holds; left alone, and named, where it was purged since; made anew only
// in a tenant made new for the bundle; and the installed app's where the
// app was installed again.
func TestARestorePutsRetainedDataBackAsRetained(t *testing.T) {
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"wiki":  chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false),
		"notes": chartProfile("notes", "1.0.0", gentianov1alpha1.DatabaseEngineMariaDB, false),
		"files": chartProfile("files", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false),
		"crm":   chartProfile("crm", "1.0.0", "", true),
	}
	held := map[string]liveApp{
		"notes": {held: backup.Stores{Database: gentianov1alpha1.DatabaseEngineMariaDB}},
		"files": {held: backup.Stores{Database: gentianov1alpha1.DatabaseEnginePostgreSQL}, claims: []string{"files-release-data"}},
		// crm: purged since the bundle was taken.
	}
	live := func(tenant *gentianov1alpha1.Tenant, held map[string]liveApp, known map[string]*gentianov1alpha1.ComponentProfile) func(string) (liveApp, error) {
		return func(app string) (liveApp, error) {
			now := held[app]
			now.profile = known[app]
			for _, a := range tenant.Spec.Apps {
				if a.Profile == app {
					now.installed = true
				}
			}
			return now, nil
		}
	}
	omitted := func(plan *restorePlan) map[string]string {
		out := map[string]string{}
		for _, o := range plan.notRestored {
			out[o.App] = o.Reason
		}
		return out
	}
	planned := func(plan *restorePlan) map[string]plannedApp {
		out := map[string]plannedApp{}
		for _, a := range plan.apps {
			out[a.name] = a
		}
		return out
	}

	// --- into the tenant the bundle was taken of.
	tenant := planTenant("demo", "wiki")
	plan, err := planRestore(retainedManifest(), tenant, nil, false, targetOf(tenant), live(tenant, held, profiles))
	if err != nil {
		t.Fatal(err)
	}
	apps := planned(plan)
	for _, name := range []string{"notes", "files"} {
		app, ok := apps[name]
		if !ok || !app.retained {
			t.Fatalf("%s is not put back as retained: %+v, not restored: %v", name, app, omitted(plan))
		}
		for _, a := range app.artefacts {
			if a.Claim != nil {
				t.Errorf("%s: %s is to be made in a tenant that has it", name, a.Target)
			}
		}
		if !strings.Contains(app.note, "the app is not installed") {
			t.Errorf("%s: note = %q", name, app.note)
		}
	}
	if got := apps["notes"].artefacts[0].Target; got != "demo_notes" {
		t.Errorf("notes' database goes to %q", got)
	}
	if apps["wiki"].retained {
		t.Error("the installed app is put back as retained")
	}
	if why := omitted(plan)["crm"]; !strings.Contains(why, "purged since") || !strings.Contains(why, "a purge is for good") {
		t.Errorf("the purged app: %q", why)
	}

	// A store of it that is gone leaves the whole app as it is: an app is
	// restored whole or not at all.
	partly := map[string]liveApp{"files": {held: backup.Stores{Database: gentianov1alpha1.DatabaseEnginePostgreSQL}}}
	plan, err = planRestore(retainedManifest(), tenant, nil, false, targetOf(tenant), live(tenant, partly, profiles))
	if err != nil {
		t.Fatal(err)
	}
	if why := omitted(plan)["files"]; !strings.Contains(why, "no longer holds its volume claim files-release-data") {
		t.Errorf("files, with its claim purged: %q; planned %v", why, planned(plan)["files"])
	}

	// --- into a tenant made new for the bundle: the stores are made.
	fresh := planTenant("demo", "wiki")
	target := targetOf(fresh)
	target.intoNewTenant = true
	plan, err = planRestore(retainedManifest(), fresh, nil, false, target, live(fresh, nil, profiles))
	if err != nil {
		t.Fatal(err)
	}
	apps = planned(plan)
	for _, name := range []string{"notes", "files", "crm"} {
		if app, ok := apps[name]; !ok || !app.retained {
			t.Fatalf("an import does not bring %s as retained: not restored %v", name, omitted(plan))
		}
	}
	volume := apps["files"].artefacts[1]
	if volume.Claim == nil || volume.Claim.Size != "3Gi" || volume.Target != "files-release-data" {
		t.Errorf("the claim to be made: %+v", volume)
	}
	if !strings.Contains(apps["files"].note, "default storage class") {
		t.Errorf("files: note = %q", apps["files"].note)
	}
	// Without the app's definition nothing says how its stores are made.
	plan, err = planRestore(retainedManifest(), fresh, nil, false, target, live(fresh, nil,
		map[string]*gentianov1alpha1.ComponentProfile{"wiki": profiles["wiki"], "notes": profiles["notes"], "crm": profiles["crm"]}))
	if err != nil {
		t.Fatal(err)
	}
	if why := omitted(plan)["files"]; !strings.Contains(why, "no ComponentProfile") {
		t.Errorf("files without a definition: %q", why)
	}
	// And a definition that declares another engine would be given a
	// database it does not look for.
	other := map[string]*gentianov1alpha1.ComponentProfile{"wiki": profiles["wiki"], "crm": profiles["crm"], "files": profiles["files"],
		"notes": chartProfile("notes", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false)}
	plan, err = planRestore(retainedManifest(), fresh, nil, false, target, live(fresh, nil, other))
	if err != nil {
		t.Fatal(err)
	}
	if why := omitted(plan)["notes"]; !strings.Contains(why, "declares a postgresql database") {
		t.Errorf("notes, whose definition declares another engine: %q", why)
	}

	// --- installed again since: the data is the installed app's, and no
	// build is on record to compare with.
	again := planTenant("demo", "wiki", "notes")
	plan, err = planRestore(retainedManifest(), again, nil, false, targetOf(again), live(again, held, profiles))
	if err != nil {
		t.Fatal(err)
	}
	if why := omitted(plan)["notes"]; !strings.Contains(why, "cannot be compared") || !strings.Contains(why, "skipVersionCheck") {
		t.Errorf("notes, installed again: %q", why)
	}
	plan, err = planRestore(retainedManifest(), again, nil, true, targetOf(again), live(again, held, profiles))
	if err != nil {
		t.Fatal(err)
	}
	if app := planned(plan)["notes"]; app.retained || !strings.Contains(app.note, "installed here now") {
		t.Errorf("notes, installed again and restored: %+v", app)
	}
}

// A restore into a tenant made new for the bundle makes the stores an
// uninstalled app's data goes into, installs nothing, and leaves the tenant
// holding the data as an uninstall would have: on record, with the claim
// that carries its release -- which is what the read of what uninstalled
// apps hold, the next install and a purge find it by.
func TestAnImportBringsRetainedDataWithoutInstallingTheApp(t *testing.T) {
	w := newRestoreWorld(t, func(r *gentianov1alpha1.TenantRestore) { r.Spec.IntoNewTenant = true })
	ctx := context.Background()
	m := retainedManifest()
	m.Tenant = "old"
	m.Apps = []backup.ManifestApp{m.Apps[0], m.Apps[2], m.Apps[3]}
	m.Apps[1].Stores[0].Name, m.Apps[1].Stores[0].Path = "old_files", "postgres/old_files.pgc"
	m.Apps[1].Stores[1].Claim.Annotations["meta.helm.sh/release-namespace"] = "tenant-old"
	w.bundles.manifest = m
	for _, profile := range []*gentianov1alpha1.ComponentProfile{
		chartProfile("files", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false),
		chartProfile("crm", "1.0.0", "", true),
	} {
		if err := w.c.Create(ctx, profile); err != nil {
			t.Fatal(err)
		}
	}
	// The tenants' PostgreSQL, and a vault to seed the database's password.
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: "Cluster"})
	cluster.SetName(cnpgClusterName)
	cluster.SetNamespace(postgresNamespace)
	if err := w.c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	w.r.Tenant.Seeder = testSeeder(t)

	// The database record is the cluster's to make ready; here the test is.
	ready := func() {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgDatabaseKind + "List"})
		if err := w.c.List(ctx, list, client.InNamespace(postgresNamespace)); err != nil {
			t.Fatal(err)
		}
		for i := range list.Items {
			if !cnpgDatabaseIsReady(&list.Items[i]) {
				_ = unstructured.SetNestedField(list.Items[i].Object, true, "status", "applied")
				if err := w.c.Update(ctx, &list.Items[i]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	var got *gentianov1alpha1.TenantRestore
	for i := 0; i < 60; i++ {
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
		ready()
		if got = w.restore(t); got.IsTerminal() {
			break
		}
	}
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status)
	}
	if len(got.Status.NotRestored) != 0 || got.Status.Complete == nil || !*got.Status.Complete {
		t.Errorf("not restored = %+v, complete = %v", got.Status.NotRestored, got.Status.Complete)
	}

	// Nothing was installed.
	tenant := &gentianov1alpha1.Tenant{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "demo"}, tenant); err != nil {
		t.Fatal(err)
	}
	if apps := backup.TenantApps(tenant); apps["files"] || apps["crm"] {
		t.Errorf("the restore installed an app: %v", tenant.Spec.Apps)
	}
	if len(got.Status.Quiesced) != 0 {
		t.Errorf("quiesced = %v", got.Status.Quiesced)
	}

	// The stores are on record, under this tenant's names.
	recorded, err := w.r.Tenant.provisionedStores(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p := recorded["files"]; p.DatabaseEngine != gentianov1alpha1.DatabaseEnginePostgreSQL || p.Database != "demo_files" || p.DatabaseUser != "demo_files" {
		t.Errorf("the record for files = %+v", p)
	}
	if p := recorded["crm"]; p.Bucket != "demo-crm" {
		t.Errorf("the record for crm = %+v", p)
	}
	// The database was made by the Job install makes it by, and recorded.
	jobs := w.jobs(t)
	if _, ok := jobs[roleJobName("demo", "files")]; !ok {
		t.Errorf("no role Job for the retained app's database: %v", jobNames(jobs))
	}
	record := databaseRecord("demo", "files")
	if err := w.c.Get(ctx, client.ObjectKeyFromObject(record), record); err != nil {
		t.Errorf("the cluster keeps no record of the retained app's database: %v", err)
	}
	// The claim, as it was, in this tenant's namespace.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "files-release-data", Namespace: "tenant-demo"}, pvc); err != nil {
		t.Fatalf("the retained app's claim was not made: %v", err)
	}
	if size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; size.String() != "3Gi" || pvc.Spec.StorageClassName != nil {
		t.Errorf("claim: size %s, class %v", size.String(), pvc.Spec.StorageClassName)
	}
	if pvc.Annotations["meta.helm.sh/release-name"] != "files-release" || pvc.Annotations["meta.helm.sh/release-namespace"] != "tenant-demo" {
		t.Errorf("the claim does not say whose it is here: %v", pvc.Annotations)
	}
	// Each artefact was loaded by the unit an installed app's is, from the
	// bundle's name for it into this tenant's.
	var restored []string
	for name, job := range jobs {
		if job.Labels[backup.ExportLabel] == "r1" && job.Labels[appLabel] == "files" {
			restored = append(restored, name)
			if job.Namespace == postgresNamespace {
				script := containerArgs(job)
				if !strings.Contains(script, "postgres/old_files.pgc") || !strings.Contains(script, "DB='demo_files'") {
					t.Errorf("files' database is not loaded from the bundle's artefact into this tenant's database:\n%s", script)
				}
			}
		}
	}
	if len(restored) != 2 {
		t.Errorf("files was restored by %v, want a database unit and a volume unit", restored)
	}

	// And the tenant now holds the two apps as retained: what an export of
	// it captures, by the one rule the read of retained data goes by too.
	_, names, err := w.r.Reconciler.retainedSet(ctx, tenant, &gentianov1alpha1.TenantExport{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"crm", "files"}) {
		t.Errorf("after the import the tenant holds as retained: %v", names)
	}
	if notes := strings.Join(got.Status.Notes, "\n"); !strings.Contains(notes, "files: its data was put back as an uninstalled app's") {
		t.Errorf("the result does not say files came back as retained:\n%s", notes)
	}
}

func jobNames(jobs map[string]*batchv1.Job) []string {
	var out []string
	for name := range jobs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func containerArgs(job *batchv1.Job) string {
	var b strings.Builder
	for _, group := range [][]corev1.Container{job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers} {
		for _, c := range group {
			b.WriteString(strings.Join(c.Command, " "))
			b.WriteString(strings.Join(c.Args, " "))
		}
	}
	return b.String()
}

// --- gap 2: the desktop's database on the kernel's PostgreSQL ----------------

// The tenant that adopts the kernel realm keeps its desktop's database on
// the kernel's PostgreSQL. It is captured there, as the database's owner,
// named in the manifest, put back there, and emptied when the tenant is
// deleted. An export used to leave it out and say so.
func TestTheDesktopDatabaseOnTheKernelsPostgreSQLIsCapturedRestoredAndEmptied(t *testing.T) {
	w := newGapsWorld(t)
	ctx := context.Background()
	w.tr.KernelRealm = "kernel"
	platform := planTenant("platform")
	platform.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	w.export.Namespace = "tenant-platform"

	units, err := w.er.tenantWideUnits(ctx, platform, w.export, gapsEncryption)
	if err != nil {
		t.Fatal(err)
	}
	var shell *captureUnit
	for i := range units {
		if units[i].Kind == bundle.ArtefactPostgres {
			shell = &units[i]
		}
	}
	if shell == nil {
		t.Fatal("no unit captures the desktop's database of the tenant that adopts the kernel realm")
	}
	if shell.Job.Namespace != "kernel-data" || shell.Name != "portal_shell" || shell.Path != "postgres/portal_shell.pgc" {
		t.Errorf("the unit: %s in %s, to %s", shell.Name, shell.Job.Namespace, shell.Path)
	}
	script := containerArgs(shell.Job)
	if !strings.Contains(script, "--dbname='portal_shell'") {
		t.Errorf("the unit does not dump the kernel's desktop database:\n%s", script)
	}
	for _, c := range shell.Job.Spec.Template.Spec.InitContainers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == backup.PostgresAdminSecret {
				t.Error("the unit reads an administrator's Secret that is not beside the kernel's PostgreSQL")
			}
		}
	}
	// The bundle's own credentials are staged beside it, and removed again.
	if !slices.Contains(unitNamespaces(units), "kernel-data") || !slices.Contains(runNamespaces("tenant-platform"), "kernel-data") {
		t.Error("the run's credentials are not staged, or not removed, beside the kernel's PostgreSQL")
	}
	if got, err := w.er.notIncluded(ctx, platform, &gentianov1alpha1.TenantExport{}); err != nil || len(got) != 0 {
		t.Errorf("notIncluded = %v, %v", got, err)
	}
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: backupTenantComponent, Artefacts: unitArtefacts(units)}}
	m := w.er.buildManifest(w.export, platform)
	if m.Shell == nil || m.Shell.Name != "portal_shell" || m.Shell.Path != "postgres/portal_shell.pgc" {
		t.Fatalf("the manifest's desktop database: %+v", m.Shell)
	}

	// An ordinary tenant's is where it was: beside the tenants' PostgreSQL.
	ordinary, err := w.er.tenantWideUnits(ctx, w.tenant, w.export, gapsEncryption)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range ordinary {
		if u.Kind == bundle.ArtefactPostgres && (u.Job.Namespace != postgresNamespace || u.Name != "demo_shell") {
			t.Errorf("an ordinary tenant's desktop database: %s in %s", u.Name, u.Job.Namespace)
		}
	}

	// A restore puts it back where the tenant restored into keeps it.
	rr := &TenantRestoreReconciler{Client: w.c, Tenant: w.tr, Reconciler: w.er}
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-platform"},
		Status: gentianov1alpha1.TenantRestoreStatus{Bundle: &gentianov1alpha1.BundleRef{Bucket: "b", Prefix: "p"}}}
	target, err := rr.planTarget(ctx, platform, restore)
	if err != nil || target.desktopDatabase != "portal_shell" {
		t.Fatalf("target = %+v, %v", target, err)
	}
	m.Apps = []backup.ManifestApp{{Name: "wiki", Stores: []backup.ManifestStore{{Kind: bundle.ArtefactPostgres, Name: "x", Path: "postgres/x.pgc"}}}}
	plan, err := planRestore(m, platform, nil, true, target, func(string) (liveApp, error) {
		return liveApp{installed: true, profile: chartProfile("wiki", "", gentianov1alpha1.DatabaseEnginePostgreSQL, false)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recordPlan(restore, plan)
	d := backup.Decryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, SecretName: "k", SecretKey: "passphrase"}
	back, err := rr.tenantWideRestoreUnits(ctx, platform, restore, d)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range back {
		if u.Kind != bundle.ArtefactPostgres {
			continue
		}
		found = true
		script := containerArgs(u.Job)
		if u.Job.Namespace != "kernel-data" || !strings.Contains(script, "postgres/portal_shell.pgc") || strings.Contains(script, "--role") {
			t.Errorf("the restore unit: in %s\n%s", u.Job.Namespace, script)
		}
	}
	if !found {
		t.Error("no unit puts the desktop's database back")
	}

	// Deleted with the tenant: emptied by a Job that has to succeed, with
	// deletionPolicy Delete and for this tenant only.
	platform.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyRetain
	if err := w.tr.deleteKernelDesktop(ctx, platform); err != nil {
		t.Fatalf("retain: %v", err)
	}
	platform.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	if err := w.tr.deleteKernelDesktop(ctx, platform); err != errDeleteJobPending {
		t.Fatalf("the deletion does not wait for the database to be emptied: %v", err)
	}
	job := &batchv1.Job{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "pg-empty-platform-shell", Namespace: "kernel-data"}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	if err := w.c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := w.tr.deleteKernelDesktop(ctx, platform); err == nil || err == errDeleteJobPending {
		t.Errorf("a Job that failed to empty the database does not fail the deletion: %v", err)
	}
	w.tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	if err := w.tr.deleteKernelDesktop(ctx, w.tenant); err != nil {
		t.Errorf("an ordinary tenant's deletion touches the kernel's PostgreSQL: %v", err)
	}
	// An ordinary tenant's desktop database is dropped with its apps': the
	// cluster's record of it is among the databases a deletion collects.
	apps, err := w.tr.collectPostgresAppsForDelete(ctx, w.tenant)
	if err != nil || !slices.Contains(apps, backup.DesktopStore) {
		t.Errorf("a deletion drops the PostgreSQL databases of %v, %v: not the desktop's", apps, err)
	}
}

// --- gap 3: mailboxes --------------------------------------------------------

// On a cluster that runs its own mail server a tenant's mailboxes are
// captured, put back, and destroyed with the tenant; on one that does not
// there is nothing to do and nothing is said to be missing.
func TestATenantsMailboxesAreCapturedRestoredAndDestroyed(t *testing.T) {
	ctx := context.Background()

	// --- no mail server: no unit, no error, nothing left out.
	w := newGapsWorld(t)
	units, err := w.er.tenantWideUnits(ctx, w.tenant, w.export, gapsEncryption)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		if u.Kind == bundle.ArtefactMailboxes {
			t.Error("mailboxes are captured on a cluster that keeps none")
		}
	}
	if got, err := w.er.notIncluded(ctx, w.tenant, w.export); err != nil || len(got) != 0 {
		t.Errorf("notIncluded = %v, %v", got, err)
	}
	w.tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	if err := w.tr.deleteMailboxes(ctx, w.tenant); err != nil {
		t.Errorf("a deletion on a cluster without a mail server: %v", err)
	}

	// --- a mail server: a unit beside its volume, on the node it is held on.
	w = newGapsWorld(t, mailCluster()...)
	w.tr.MailServiceMode = mailServiceModeSystem
	units, err = w.er.tenantWideUnits(ctx, w.tenant, w.export, gapsEncryption)
	if err != nil {
		t.Fatal(err)
	}
	var mail *captureUnit
	for i := range units {
		if units[i].Kind == bundle.ArtefactMailboxes {
			mail = &units[i]
		}
	}
	if mail == nil {
		t.Fatal("no unit captures the tenant's mailboxes")
	}
	if mail.Name != "demo.k.example" || mail.Path != backup.MailboxesArtefact || mail.Job.Namespace != mailNamespace {
		t.Errorf("the unit: %s to %s in %s", mail.Name, mail.Path, mail.Job.Namespace)
	}
	pod := mail.Job.Spec.Template.Spec
	if pod.NodeSelector[corev1.LabelHostname] != "node-7" || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "dovecot-dev-mail" {
		t.Errorf("the unit is not beside the mail server's volume: %v %v", pod.NodeSelector, pod.Volumes[0])
	}
	if !strings.Contains(containerArgs(mail.Job), `backup "maildir:`) {
		t.Error("the unit does not copy by the mail server's own synchronisation")
	}
	if !slices.Contains(unitNamespaces(units), mailNamespace) || !slices.Contains(runNamespaces("tenant-demo"), mailNamespace) {
		t.Error("the run's credentials are not staged, or not removed, beside the mail server")
	}
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: backupTenantComponent, Artefacts: unitArtefacts(units)}}
	m := w.er.buildManifest(w.export, w.tenant)
	if m.Mailboxes == nil || m.Mailboxes.Name != "demo.k.example" || m.Mailboxes.Path != "mail/mailboxes.tar.gz" {
		t.Fatalf("the manifest's mailboxes: %+v", m.Mailboxes)
	}

	// --- restored into the mailboxes of the tenant restored into, whatever
	// its domain: here a tenant of another name.
	m.Apps = []backup.ManifestApp{{Name: "wiki", Stores: []backup.ManifestStore{{Kind: bundle.ArtefactPostgres, Name: "x", Path: "postgres/x.pgc"}}}}
	other := planTenant("other", "wiki")
	rr := &TenantRestoreReconciler{Client: w.c, Tenant: w.tr, Reconciler: w.er}
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-other"},
		Status: gentianov1alpha1.TenantRestoreStatus{Bundle: &gentianov1alpha1.BundleRef{Bucket: "b", Prefix: "p"}}}
	target, err := rr.planTarget(ctx, other, restore)
	if err != nil || target.mailDomain != "other.k.example" {
		t.Fatalf("target = %+v, %v", target, err)
	}
	installed := func(string) (liveApp, error) {
		return liveApp{installed: true, profile: chartProfile("wiki", "", gentianov1alpha1.DatabaseEnginePostgreSQL, false)}, nil
	}
	plan, err := planRestore(m, other, nil, true, target, installed)
	if err != nil {
		t.Fatal(err)
	}
	recordPlan(restore, plan)
	d := backup.Decryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, SecretName: "k", SecretKey: "passphrase"}
	back, err := rr.tenantWideRestoreUnits(ctx, other, restore, d)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range back {
		if u.Kind != bundle.ArtefactMailboxes {
			continue
		}
		found = true
		script := containerArgs(u.Job)
		var domain string
		for _, e := range u.Job.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "DOMAIN" {
				domain = e.Value
			}
		}
		if u.Job.Namespace != mailNamespace || domain != "other.k.example" || !strings.Contains(script, "mail/mailboxes.tar.gz") ||
			u.Job.Spec.Template.Spec.NodeSelector[corev1.LabelHostname] != "node-7" {
			t.Errorf("the restore unit: in %s, into %s\n%s", u.Job.Namespace, domain, script)
		}
	}
	if !found {
		t.Error("no unit puts the mailboxes back")
	}
	// Into a cluster that keeps no mailboxes they are not put back, and the
	// result says so: nothing a bundle holds is dropped without a word.
	plan, err = planRestore(m, other, nil, true, targetOf(other), installed)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.tenantWide {
		if a.Kind == bundle.ArtefactMailboxes {
			t.Error("mailboxes are planned into a cluster that keeps none")
		}
	}
	if notes := strings.Join(plan.notes, "\n"); !strings.Contains(notes, "mailboxes of demo.k.example, and they were NOT put back") {
		t.Errorf("the plan does not say the mailboxes were left out:\n%s", notes)
	}

	// --- destroyed with the tenant, with deletionPolicy Delete, by a Job
	// that has to succeed.
	w.tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyRetain
	if err := w.tr.deleteMailboxes(ctx, w.tenant); err != nil {
		t.Fatalf("retain: %v", err)
	}
	jobs := &batchv1.JobList{}
	if err := w.c.List(ctx, jobs, client.InNamespace(mailNamespace)); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("retiring a tenant ran %d Job(s) on its mailboxes, %v", len(jobs.Items), err)
	}
	w.tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	if err := w.tr.deleteMailboxes(ctx, w.tenant); err != errDeleteJobPending {
		t.Fatalf("the deletion does not wait for the mailboxes to be destroyed: %v", err)
	}
	job := &batchv1.Job{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "mail-delete-demo", Namespace: mailNamespace}, job); err != nil {
		t.Fatal(err)
	}
	var domain string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "DOMAIN" {
			domain = e.Value
		}
	}
	if domain != "demo.k.example" || job.Spec.Template.Spec.NodeSelector[corev1.LabelHostname] != "node-7" {
		t.Errorf("the destroy Job: domain %q, node %v", domain, job.Spec.Template.Spec.NodeSelector)
	}
	// Loudly: a Job that failed fails the deletion, and is run again.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	if err := w.c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := w.tr.deleteMailboxes(ctx, w.tenant); err == nil || err == errDeleteJobPending || !strings.Contains(err.Error(), "mail-delete-demo") {
		t.Errorf("a Job that failed to destroy the mailboxes does not fail the deletion: %v", err)
	}
	if err := w.tr.deleteMailboxes(ctx, w.tenant); err != errDeleteJobPending {
		t.Errorf("the failed Job is not run again: %v", err)
	}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "mail-delete-demo", Namespace: mailNamespace}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := w.c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := w.tr.deleteMailboxes(ctx, w.tenant); err != nil {
		t.Errorf("after the Job succeeded: %v", err)
	}

	// --- a mail server whose volume is not there is not "no mail".
	if err := w.c.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dovecot-dev", Namespace: mailNamespace}}); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "dovecot-dev-mail", Namespace: mailNamespace}}
	if err := w.c.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := w.er.tenantWideUnits(ctx, w.tenant, w.export, gapsEncryption); err == nil || !strings.Contains(err.Error(), "is not there") {
		t.Errorf("an export on a cluster whose mail volume is missing: %v", err)
	}
	if err := w.tr.deleteMailboxes(ctx, w.tenant); err == nil || err == errDeleteJobPending {
		t.Errorf("a deletion on a cluster whose mail volume is missing reports no error: %v", err)
	}
	// A cluster that is to run a mail server and has none deployed yet has
	// no mailboxes: nothing to do, and no error.
	if err := w.c.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dovecot-dev", Namespace: mailNamespace}}); err != nil {
		t.Fatal(err)
	}
	if boxes, why, err := w.tr.mailboxesOf(ctx, w.tenant); boxes != nil || why != "" || err != nil {
		t.Errorf("a cluster with no mail server deployed: %+v, %q, %v", boxes, why, err)
	}
}

// A mail domain the tenant shares with the cluster is not the tenant's to
// copy or to destroy: on a single-tenancy cluster the user tenant's
// addresses are on the cluster's own domain, with the administrators'.
func TestMailboxesOnTheClustersOwnDomainAreNeitherCopiedNorDestroyed(t *testing.T) {
	ctx := context.Background()
	w := newGapsWorld(t, mailCluster()...)
	w.tr.MailServiceMode = mailServiceModeSystem
	w.tr.TenancyMode = gentianov1alpha1.TenancyModeSingle
	user := planTenant(gentianov1alpha1.SingleUserTenantName)
	if got := mailDomain(user, w.tr.KernelDomain, w.tr.TenancyMode); got != "k.example" {
		t.Fatalf("the user tenant's mail domain on a single-tenancy cluster = %q", got)
	}
	boxes, why, err := w.tr.mailboxesOf(ctx, user)
	if err != nil || boxes != nil || !strings.Contains(why, "the cluster's own") {
		t.Fatalf("mailboxesOf = %+v, %q, %v", boxes, why, err)
	}
	w.export.Namespace = "tenant-" + user.Name
	units, err := w.er.tenantWideUnits(ctx, user, w.export, gapsEncryption)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		if u.Kind == bundle.ArtefactMailboxes {
			t.Error("the cluster's own mail domain is copied into a tenant's bundle")
		}
	}
	got, err := w.er.notIncluded(ctx, user, w.export)
	if err != nil || len(got) != 1 || !strings.Contains(got[0], "mailboxes of k.example") {
		t.Errorf("the export does not say it left the mailboxes out: %v, %v", got, err)
	}
	user.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	if err := w.tr.deleteMailboxes(ctx, user); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := w.c.List(ctx, jobs, client.InNamespace(mailNamespace)); err != nil || len(jobs.Items) != 0 {
		t.Errorf("deleting the tenant ran %d Job(s) on the cluster's own mail domain, %v", len(jobs.Items), err)
	}
}

// A tenant's deletion destroys its mailboxes after its mail routing is
// gone: nothing may be delivered into a mailbox that is being destroyed.
func TestMailboxesAreDestroyedAfterTheMailRoutingIsGone(t *testing.T) {
	source, err := os.ReadFile("tenant_controller.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	body = body[strings.Index(body, "func (r *TenantReconciler) reconcileDelete("):]
	routing, boxes, namespace := strings.Index(body, "r.deleteMail(ctx, tenant)"), strings.Index(body, "r.deleteMailboxes"), strings.Index(body, "r.deleteXTenant(ctx, tenant)")
	if routing < 0 || boxes < routing || namespace < boxes {
		t.Errorf("a deletion removes the mail routing at %d, destroys the mailboxes at %d and goes on to the namespace at %d", routing, boxes, namespace)
	}
	if !strings.Contains(body[boxes-400:boxes+200], "awaitJob(step(ctx, tenant))") {
		t.Error("a deletion does not wait for the Job that destroys the mailboxes")
	}
}

// --- gap 4: the rights that follow from nothing else -------------------------

// An export carries exactly the entries of the rights store the projection
// would not write, and the defaults the store no longer holds; nothing that
// is derived again, and nobody's membership.
func TestAnExportCarriesTheRightsThatFollowFromNothingElse(t *testing.T) {
	w := newGapsWorld(t)
	ctx := context.Background()

	// Nothing but the projection's: the manifest says the store was read
	// and holds nothing.
	rights, err := w.er.storeOnlyRights(ctx, w.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if rights == nil || rights.Cluster != "c1" || len(rights.Granted) != 0 || len(rights.Withdrawn) != 0 {
		t.Fatalf("rights of a tenant with the defaults only = %+v", rights)
	}

	// A right granted beyond the defaults, on the tenant and on two of its
	// apps -- one installed, one uninstalled with its data kept; the consent
	// to being operated withdrawn; and what is not the tenant's backup's to
	// carry: a membership, another tenant's entry, a stale entitlement.
	grants := []authz.Tuple{
		{User: "group:gentian/platform/support#member", Relation: "admin", Object: "tenant:demo"},
		{User: "group:gentian/tenant/demo/leads#member", Relation: "admin", Object: "app:demo/wiki"},
		{User: "group:gentian/tenant/demo/leads#member", Relation: "admin", Object: "app:demo/notes"},
	}
	for _, t := range grants {
		w.rights.tuples[t] = true
	}
	delete(w.rights.tuples, authz.Tuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"})
	for _, t := range []authz.Tuple{
		{User: "user:1234", Relation: "member", Object: "group:gentian/tenant/demo/admins"},
		{User: "group:gentian/platform/support#member", Relation: "admin", Object: "tenant:other"},
		{User: "group:gentian/tenant/demo/app/gone#member", Relation: "entitled", Object: "app:demo/notes"},
	} {
		w.rights.tuples[t] = true
	}
	rights, err = w.er.storeOnlyRights(ctx, w.tenant)
	if err != nil {
		t.Fatal(err)
	}
	var granted []string
	for _, g := range rights.Granted {
		granted = append(granted, g.User+" "+g.Relation+" "+g.Object)
	}
	want := []string{
		"group:gentian/platform/support#member admin tenant:demo",
		"group:gentian/tenant/demo/leads#member admin app:demo/notes",
		"group:gentian/tenant/demo/leads#member admin app:demo/wiki",
	}
	if !reflect.DeepEqual(granted, want) {
		t.Errorf("granted = %v\nwant %v", granted, want)
	}
	if len(rights.Withdrawn) != 1 || rights.Withdrawn[0] != (bundle.RightsTuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"}) {
		t.Errorf("withdrawn = %+v", rights.Withdrawn)
	}

	// They travel in the manifest, which is encrypted like every artefact.
	unit, err := w.er.manifestUnit(w.export, w.tenant, gapsEncryption, rights)
	if err != nil {
		t.Fatal(err)
	}
	if staged := containerArgs(unit.Job); !strings.Contains(staged, `"rights":{"cluster":"c1","granted":[`) || !strings.Contains(staged, `"withdrawn":[{"user":"cluster:c1","relation":"operated_by","object":"tenant:demo"}]`) {
		t.Errorf("the manifest does not hold the rights:\n%s", staged)
	}

	// A tenant the projection has not attached yet has nothing withdrawn:
	// the export waits instead of recording every default as taken away.
	w.tr.Rights = newFakeRights()
	if _, err := w.er.storeOnlyRights(ctx, w.tenant); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Errorf("the rights of a tenant the store does not know yet: %v", err)
	}
	// A cluster that runs no rights store: nothing to carry, no error.
	w.tr.Rights = nil
	if rights, err := w.er.storeOnlyRights(ctx, w.tenant); err != nil || rights != nil {
		t.Errorf("without a rights store: %+v, %v", rights, err)
	}
}

func rightsManifest(tenant, cluster string) *backup.Manifest {
	return &backup.Manifest{
		SchemaVersion: 3, Tenant: tenant,
		Apps: []backup.ManifestApp{{Name: "wiki", Stores: []backup.ManifestStore{{Kind: bundle.ArtefactPostgres, Name: "x", Path: "postgres/x.pgc"}}}},
		Rights: &bundle.ManifestRights{
			Cluster: cluster,
			Granted: []bundle.RightsTuple{
				{User: "group:gentian/platform/support#member", Relation: "admin", Object: "tenant:" + tenant},
				{User: "group:gentian/tenant/" + tenant + "/leads#member", Relation: "admin", Object: "app:" + tenant + "/wiki"},
			},
			Withdrawn: []bundle.RightsTuple{{User: "cluster:" + cluster, Relation: "operated_by", Object: "tenant:" + tenant}},
		},
	}
}

// A restore into the tenant the bundle was taken of puts the rights back.
// An import -- a tenant made new, under any name, on any cluster -- brings
// no right that was granted, names each, and withdraws what was withdrawn,
// under the new tenant's names.
func TestARestorePutsRightsBackAndAnImportBringsNoGrant(t *testing.T) {
	installed := func(string) (liveApp, error) {
		return liveApp{installed: true, profile: chartProfile("wiki", "", gentianov1alpha1.DatabaseEnginePostgreSQL, false)}, nil
	}
	target := func(tenant *gentianov1alpha1.Tenant, cluster string, intoNew bool) planTarget {
		out := targetOf(tenant)
		out.cluster, out.haveRights, out.intoNewTenant = cluster, true, intoNew
		return out
	}

	// --- a restore, same tenant, same cluster.
	demo := planTenant("demo", "wiki")
	plan, err := planRestore(rightsManifest("demo", "c1"), demo, nil, true, target(demo, "c1", false), installed)
	if err != nil {
		t.Fatal(err)
	}
	r := plan.rights
	if r == nil || len(r.Grant) != 2 || len(r.Withdraw) != 1 || len(r.NotBrought) != 0 {
		t.Fatalf("a restore's rights = %+v", r)
	}
	if r.Withdraw[0] != "cluster:c1 operated_by tenant:demo" || r.Grant[1] != "group:gentian/tenant/demo/leads#member admin app:demo/wiki" {
		t.Errorf("rights = %+v", r)
	}

	// --- an import, in each of the three ways a tenant is new.
	renamed := planTenant("acme", "wiki")
	cases := map[string]struct {
		tenant   *gentianov1alpha1.Tenant
		target   planTarget
		withdraw string
		named    string
	}{
		"said to be new":  {demo, target(demo, "c1", true), "cluster:c1 operated_by tenant:demo", "group:gentian/platform/support#member admin tenant:demo"},
		"another name":    {renamed, target(renamed, "c1", false), "cluster:c1 operated_by tenant:acme", "group:gentian/tenant/acme/leads#member admin app:acme/wiki"},
		"another cluster": {demo, target(demo, "c2", false), "cluster:c2 operated_by tenant:demo", "group:gentian/platform/support#member admin tenant:demo"},
	}
	for name, c := range cases {
		plan, err := planRestore(rightsManifest("demo", "c1"), c.tenant, nil, true, c.target, installed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := plan.rights
		if r == nil || len(r.Grant) != 0 {
			t.Errorf("%s: an import grants %v", name, r)
			continue
		}
		if len(r.Withdraw) != 1 || r.Withdraw[0] != c.withdraw {
			t.Errorf("%s: withdraws %v, want %s", name, r.Withdraw, c.withdraw)
		}
		left := strings.Join(r.NotBrought, "\n")
		if len(r.NotBrought) != 2 || !strings.Contains(left, c.named) || !strings.Contains(left, "not carried into a tenant made new") {
			t.Errorf("%s: not brought:\n%s", name, left)
		}
		// Said in the result, entry by entry.
		restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-" + c.tenant.Name}}
		recordPlan(restore, plan)
		if notes := strings.Join(restore.Status.Notes, "\n"); strings.Count(notes, "A right the bundle holds was NOT brought") != 2 {
			t.Errorf("%s: the result does not name what was not brought:\n%s", name, notes)
		}
	}

	// --- what is never written, wherever the bundle is restored: an entry
	// on something that is not the tenant's, a person by an identifier of
	// the old realm, and what is no id of the store at all.
	m := rightsManifest("demo", "c1")
	m.Rights.Granted = []bundle.RightsTuple{
		{User: "group:gentian/tenant/demo/admins#member", Relation: "admin", Object: "tenant:other"},
		{User: "group:gentian/tenant/demo/admins#member", Relation: "admin", Object: "cluster:c1"},
		{User: "user:1234", Relation: "admin", Object: "tenant:demo"},
		{User: "user:*", Relation: "admin", Object: "tenant:demo"},
		{User: "group:x#member extra", Relation: "admin", Object: "tenant:demo"},
		{User: "group:gentian/tenant/demo/admins#member", Relation: "admin", Object: "app:demo/"},
	}
	m.Rights.Withdrawn = []bundle.RightsTuple{{User: "cluster:c1", Relation: "cluster", Object: "tenant:other"}}
	plan, err = planRestore(m, demo, nil, true, target(demo, "c1", false), installed)
	if err != nil {
		t.Fatal(err)
	}
	if r := plan.rights; len(r.Grant) != 0 || len(r.Withdraw) != 0 || len(r.NotBrought) != 7 {
		t.Errorf("entries a restore must not write: %+v", r)
	}

	// A cluster without a rights store writes none and says so.
	none := targetOf(demo)
	plan, err = planRestore(rightsManifest("demo", "c1"), demo, nil, true, none, installed)
	if err != nil {
		t.Fatal(err)
	}
	if r := plan.rights; len(r.Grant) != 0 || len(r.Withdraw) != 0 || len(r.NotBrought) != 3 || !strings.Contains(r.NotBrought[0], "no rights store") {
		t.Errorf("without a rights store: %+v", r)
	}
}

// The rights are written after the projection has attached the tenant: the
// projection writes the defaults, and what a bundle says was withdrawn is a
// default to take away again.
func TestRightsArePutBackAfterTheProjectionHasRun(t *testing.T) {
	ctx := context.Background()
	plan := &gentianov1alpha1.RestoreRights{
		Grant:    []string{"group:gentian/platform/support#member admin tenant:demo"},
		Withdraw: []string{"cluster:c1 operated_by tenant:demo"},
	}
	// Not attached yet: nothing is written.
	store := newFakeRights()
	done, err := applyRights(ctx, store, "c1", "demo", plan)
	if err != nil || done || store.writes != 0 || plan.Applied {
		t.Fatalf("before the projection: done %v, writes %d, %v", done, store.writes, err)
	}
	// Projected: the grant is written, the default taken away, once.
	store = newFakeRights(projected("c1", "demo", "wiki")...)
	done, err = applyRights(ctx, store, "c1", "demo", plan)
	if err != nil || !done || !plan.Applied {
		t.Fatalf("after the projection: done %v, %v", done, err)
	}
	if !store.has("group:gentian/platform/support#member", "admin", "tenant:demo") || store.has("cluster:c1", "operated_by", "tenant:demo") {
		t.Errorf("the store after the restore: %v", store.tuples)
	}
	if !store.has("cluster:c1", "cluster", "tenant:demo") || !store.has("group:gentian/tenant/demo/admins#member", "admin", "tenant:demo") {
		t.Error("the restore removed what the projection wrote")
	}
	// Run again it finds its own work: the store refuses to write what it
	// holds and to remove what it does not.
	plan.Applied = false
	before := store.writes
	if done, err := applyRights(ctx, store, "c1", "demo", plan); err != nil || !done || store.writes != before {
		t.Errorf("a second pass: done %v, writes %d -> %d, %v", done, before, store.writes, err)
	}
	// An entry that is not on the tenant is refused here too: the status a
	// restore works from can be written by whoever can write a status.
	bad := &gentianov1alpha1.RestoreRights{Grant: []string{"group:x#member admin tenant:other"}}
	if _, err := applyRights(ctx, store, "c1", "demo", bad); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("an entry on another tenant: %v", err)
	}
	// Nothing to do is done.
	if done, err := applyRights(ctx, nil, "c1", "demo", nil); err != nil || !done {
		t.Errorf("no rights: %v %v", done, err)
	}
}

// A whole restore with rights: they are written last, once the realm's unit
// is done, and the restore does not end before.
func TestARestoreWritesTheRightsLast(t *testing.T) {
	w := newRestoreWorld(t, func(r *gentianov1alpha1.TenantRestore) { r.Name = "r1" })
	store := newFakeRights()
	w.r.Tenant.Rights, w.r.Tenant.ClusterID = store, "c1"
	m := rightsManifest("demo", "c1")
	m.Identity = &backup.ManifestIdentity{Realm: "demo", Path: backup.IdentityArtefact}
	m.Apps[0] = backup.ManifestApp{Name: "wiki", ChartVersion: "2.0.0", Stores: []backup.ManifestStore{
		{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"}}}
	w.bundles.manifest = m
	ctx := context.Background()

	finish := func() {
		for _, job := range w.jobs(t) {
			if len(job.Status.Conditions) == 0 {
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				if err := w.c.Status().Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Until the projection has attached the tenant the restore waits, with
	// every unit done, and writes nothing.
	for i := 0; i < 12; i++ {
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
		finish()
	}
	got := w.restore(t)
	if got.IsTerminal() || store.writes != 0 {
		t.Fatalf("before the projection: phase %s, writes %d", got.Status.Phase, store.writes)
	}
	if entry := appStatus(&got.Status.Apps, backupTenantComponent); !strings.Contains(entry.Message, "rights store") {
		t.Errorf("the restore does not say what it waits for: %q", entry.Message)
	}
	for _, tuple := range projected("c1", "demo", "wiki") {
		store.tuples[tuple] = true
	}
	got = w.run(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady || got.Status.Rights == nil || !got.Status.Rights.Applied {
		t.Fatalf("phase %s, rights %+v", got.Status.Phase, got.Status.Rights)
	}
	if !store.has("group:gentian/tenant/demo/leads#member", "admin", "app:demo/wiki") || store.has("cluster:c1", "operated_by", "tenant:demo") {
		t.Errorf("the store after the restore: %v", store.tuples)
	}
}

// --- the format --------------------------------------------------------------

// A bundle written before format 3 still restores: a format 2 manifest, as
// the build before this one wrote it, reads and plans as it did. It holds
// no retained app, no mailboxes and no rights, and a restore of it says
// nothing about them.
func TestAFormat2BundleStillRestores(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "manifest-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := &backup.Manifest{}
	if err := json.Unmarshal(raw, m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 2 || bundle.OldestReadableSchemaVersion > 2 || bundle.SchemaVersion != 3 {
		t.Fatalf("the fixture is of format %d; this build reads %d to %d", m.SchemaVersion, bundle.OldestReadableSchemaVersion, bundle.SchemaVersion)
	}
	if !m.NamesArtefacts() || m.Mailboxes != nil || m.Rights != nil || m.Apps[0].Retained {
		t.Fatalf("a format 2 manifest read as %+v", m)
	}
	tenant := planTenant("demo", "wiki")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"wiki": chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true)}
	// Into a cluster that has everything format 3 added: a mail server and a
	// rights store.
	target := targetOf(tenant)
	target.mailDomain, target.cluster, target.haveRights = "demo.k.example", "c1", true
	plan, err := planRestore(m, tenant, nil, false, target, liveFrom(tenant, profiles, map[string][]string{"wiki": {"wiki-release-data"}}))
	if err != nil {
		t.Fatal(err)
	}
	if plan.schemaVersion != 2 || plan.derivation != gentianov1alpha1.RestoreNamesFromManifest || len(plan.notRestored) != 0 {
		t.Errorf("plan: version %d, derivation %s, not restored %v", plan.schemaVersion, plan.derivation, plan.notRestored)
	}
	want := []gentianov1alpha1.BundleArtefact{
		{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc", Target: "demo_wiki"},
		{Kind: bundle.ArtefactPostgresOwned, Name: "demo_wiki", Path: "postgres/demo_wiki.owned.tar.gz", Target: "demo_wiki"},
		{Kind: bundle.ArtefactS3, Name: "demo-wiki", Path: "s3/demo-wiki.tar.gz", Target: "demo-wiki"},
		{Kind: bundle.ArtefactVolume, Name: "wiki-release-data", Path: "volumes/wiki-release-data.tar.gz", Release: "wiki-release", Target: "wiki-release-data"},
	}
	if len(plan.apps) != 1 || plan.apps[0].retained || !reflect.DeepEqual(plan.apps[0].artefacts, want) {
		t.Errorf("planned = %+v", plan.apps)
	}
	var wide []string
	for _, a := range plan.tenantWide {
		wide = append(wide, a.Kind+" "+a.Path+" -> "+a.Target)
	}
	if !reflect.DeepEqual(wide, []string{"identity identity/realm.tar.gz -> demo", "postgres postgres/demo_shell.pgc -> demo_shell"}) {
		t.Errorf("the tenant's own = %v", wide)
	}
	if plan.rights != nil || len(plan.notes) != 0 {
		t.Errorf("a format 2 bundle is said to hold rights or mailboxes: %+v, %v", plan.rights, plan.notes)
	}
	// What that bundle said it did not hold is repeated, as before.
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-demo"}}
	recordPlan(restore, plan)
	if notes := strings.Join(restore.Status.Notes, "\n"); !strings.Contains(notes, "The bundle says it does not hold the data uninstalled apps left behind (notes,") {
		t.Errorf("the notes:\n%s", notes)
	}
	if restore.Status.BundleSchemaVersion != 2 {
		t.Errorf("status.bundleSchemaVersion = %d", restore.Status.BundleSchemaVersion)
	}
}

// --- the inventory, held to the code -----------------------------------------

// Every kind of artefact a bundle holds for a tenant is captured by an
// export, planned and put back by a restore, and -- where the inventory
// says a deletion destroys what it carries -- destroyed by a deletion. A
// kind added to the bundle's list and to one of these only fails here: this
// is what keeps the four directions on one list.
func TestEveryTenantArtefactIsCapturedPlannedRestoredAndDestroyed(t *testing.T) {
	ctx := context.Background()
	w := newGapsWorld(t, mailCluster()...)
	w.tr.MailServiceMode = mailServiceModeSystem
	w.rights.tuples[authz.Tuple{User: "group:gentian/platform/support#member", Relation: "admin", Object: "tenant:demo"}] = true

	// Captured.
	units, err := w.er.tenantWideUnits(ctx, w.tenant, w.export, gapsEncryption)
	if err != nil {
		t.Fatal(err)
	}
	rights, err := w.er.storeOnlyRights(ctx, w.tenant)
	if err != nil {
		t.Fatal(err)
	}
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{
		{Name: "wiki", ChartVersion: "2.0.0", Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"}}},
		{Name: backupTenantComponent, Artefacts: unitArtefacts(units)},
	}
	m := w.er.buildManifest(w.export, w.tenant)
	m.Rights = rights
	captured := map[string]bool{}
	for _, u := range units {
		captured[u.Kind] = true
	}
	inManifest := map[string]bool{
		bundle.ArtefactPostgres:  m.Shell != nil,
		bundle.ArtefactMailboxes: m.Mailboxes != nil,
		bundle.ArtefactIdentity:  m.Identity != nil,
		bundle.ArtefactRights:    m.Rights != nil && len(m.Rights.Granted) == 1,
	}

	// Planned and put back.
	rr := &TenantRestoreReconciler{Client: w.c, Tenant: w.tr, Reconciler: w.er}
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantRestoreStatus{Bundle: &gentianov1alpha1.BundleRef{Bucket: "b", Prefix: "p"}}}
	target, err := rr.planTarget(ctx, w.tenant, restore)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRestore(m, w.tenant, nil, false, target, liveFrom(w.tenant,
		map[string]*gentianov1alpha1.ComponentProfile{"wiki": chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, false)}, nil))
	if err != nil {
		t.Fatal(err)
	}
	recordPlan(restore, plan)
	planned := map[string]bool{bundle.ArtefactRights: plan.rights != nil && len(plan.rights.Grant) == 1}
	for _, a := range plan.tenantWide {
		planned[a.Kind] = true
	}
	back, err := rr.tenantWideRestoreUnits(ctx, w.tenant, restore, backup.Decryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, SecretName: "k", SecretKey: "passphrase"})
	if err != nil {
		t.Fatal(err)
	}
	restored := map[string]bool{}
	for _, u := range back {
		restored[u.Kind] = true
	}
	// The rights are no unit: the operator writes them.
	delete(w.rights.tuples, authz.Tuple{User: "group:gentian/platform/support#member", Relation: "admin", Object: "tenant:demo"})
	if done, err := applyRights(ctx, w.rights, "c1", "demo", restore.Status.Rights); err == nil && done {
		restored[bundle.ArtefactRights] = w.rights.has("group:gentian/platform/support#member", "admin", "tenant:demo")
	}
	captured[bundle.ArtefactRights] = inManifest[bundle.ArtefactRights]

	// Destroyed, where the inventory says so.
	w.tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	destroyed := map[string]func() bool{
		bundle.ArtefactMailboxes: func() bool {
			return w.tr.deleteMailboxes(ctx, w.tenant) == errDeleteJobPending
		},
		bundle.ArtefactPostgres: func() bool {
			apps, err := w.tr.collectPostgresAppsForDelete(ctx, w.tenant)
			return err == nil && slices.Contains(apps, backup.DesktopStore)
		},
		bundle.ArtefactIdentity: func() bool {
			job := makeRealmDeleteJob(w.tenant, "demo", "kernel")
			return job != nil && strings.Contains(containerArgs(job), "demo")
		},
	}

	for _, kind := range bundle.TenantArtefacts {
		rule := backup.TenantRuleFor(kind)
		if !captured[kind] || !inManifest[kind] {
			t.Errorf("%s (%s): captured by an export = %v, named in the manifest = %v", kind, rule.What, captured[kind], inManifest[kind])
		}
		if !planned[kind] || !restored[kind] {
			t.Errorf("%s (%s): planned by a restore = %v, put back = %v", kind, rule.What, planned[kind], restored[kind])
		}
		check, has := destroyed[kind]
		switch {
		case rule.Destroyed && !has:
			t.Errorf("%s (%s): the inventory says a deletion destroys it, and nothing here shows what does", kind, rule.What)
		case rule.Destroyed && !check():
			t.Errorf("%s (%s): the inventory says a deletion destroys it, and the deletion does not", kind, rule.What)
		case !rule.Destroyed && has:
			t.Errorf("%s (%s): a deletion destroys it, and the inventory says it stays", kind, rule.What)
		}
	}
	// And the same for an app's: every kind of artefact a bundle holds for
	// an app is one a restore of an installed app and of a retained one
	// both know.
	for _, kind := range bundle.AppArtefacts {
		entry := gentianov1alpha1.BundleArtefact{Kind: kind, Name: "n", Path: "p/x", Target: "t"}
		restore.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{entry}}}
		units, err := rr.restoreUnits(ctx, w.tenant, "wiki", restore, backup.Decryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, SecretName: "k", SecretKey: "passphrase"})
		if err != nil || len(units) != 1 {
			t.Errorf("%s: a restore has no unit for it: %v", kind, err)
		}
		store := backup.ManifestStore{Kind: kind, Name: "n", Path: "p/x", Claim: &bundle.ManifestClaim{Size: "1Gi"}}
		profile := storesProfile("wiki", gentianov1alpha1.DatabaseEnginePostgreSQL, true, false)
		if kind == bundle.ArtefactMariaDB || kind == bundle.ArtefactMariaDBOwned {
			profile = storesProfile("wiki", gentianov1alpha1.DatabaseEngineMariaDB, true, false)
		}
		if _, _, why, err := planRetained(w.tenant, backup.ManifestApp{Name: "wiki", Retained: true, Stores: []backup.ManifestStore{store}},
			liveApp{profile: profile}, true); err != nil || why != "" {
			t.Errorf("%s: the data of an uninstalled app is not brought as it: %q, %v", kind, why, err)
		}
	}
}

// What is on record as destroyed is gone for an export too: a purge takes
// the store off the record, and with nothing held the app is in no bundle
// taken afterwards.
func TestPurgedDataIsInNoLaterBundle(t *testing.T) {
	w := newGapsWorld(t)
	ctx := context.Background()
	record := &corev1.ConfigMap{}
	if err := w.c.Get(ctx, backup.ProvisionedRecordKey("demo"), record); err != nil {
		t.Fatal(err)
	}
	// What a purge of notes and of crm does to the record, kind by kind.
	for _, forget := range [][2]string{{"notes", string(backup.KindDatabase)}, {"crm", string(backup.KindObjectStorage)}, {"crm", string(backup.KindCache)}} {
		if _, err := backup.ForgetProvisioned(record, forget[0], backup.Kind(forget[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.c.Update(ctx, record); err != nil {
		t.Fatal(err)
	}
	_, names, err := w.er.retainedSet(ctx, w.tenant, w.export)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"files"}) {
		t.Errorf("after notes and crm were purged an export captures the retained %v", names)
	}
	// An export that had begun on an app purged meanwhile fails, saying so.
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: "notes", Retained: true}}
	set, names, err := w.er.retainedSet(ctx, w.tenant, w.export)
	if err != nil || !slices.Contains(names, "notes") {
		t.Fatalf("names = %v, %v", names, err)
	}
	if err := w.c.Get(ctx, client.ObjectKeyFromObject(w.export), w.export); err != nil {
		t.Fatal(err)
	}
	w.export.Status.Bundle = &gentianov1alpha1.BundleRef{Bucket: "b", Prefix: "nightly"}
	if _, err := w.er.captureRetained(ctx, w.export, w.tenant, set["notes"], gapsEncryption); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if w.export.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed {
		t.Errorf("an export whose retained app was purged under it: phase %s", w.export.Status.Phase)
	}
}

// memoryVault is a vault for a test: what a Seeder writes its records to.
type memoryVault struct{ data map[string]map[string]string }

func (m *memoryVault) PutOnce(_ context.Context, path string, data map[string]string) error {
	if _, ok := m.data[path]; !ok {
		m.data[path] = data
	}
	return nil
}

func (m *memoryVault) Put(_ context.Context, path string, data map[string]string) error {
	m.data[path] = data
	return nil
}

func (m *memoryVault) Get(_ context.Context, path string) (map[string]string, error) {
	return m.data[path], nil
}

func testSeeder(t *testing.T) *secrets.Seeder {
	t.Helper()
	return secrets.NewSeeder(&memoryVault{data: map[string]map[string]string{}}, secrets.NewDeriver("unit-test-master"))
}
