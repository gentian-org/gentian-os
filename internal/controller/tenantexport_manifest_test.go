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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// An export captures everything a purge of the app would destroy that a
// bundle can hold: the database, every other database the app's role owns,
// the bucket and the app's own volumes -- and writes down, artefact by
// artefact, what it captured and where it put it. That record is the
// manifest a restore goes by.
func TestAnExportCapturesWhatAPurgeDestroysAndTheManifestNamesIt(t *testing.T) {
	scheme := deleteGapsScheme()
	tenant := planTenant("demo", "wiki", "wiki-pro")
	tenant.Spec.Apps[0].Digest = "sha256:aaa"
	profile := chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true)
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{
			Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"},
		},
	}
	claim := func(name, release string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-demo",
			Annotations: map[string]string{"meta.helm.sh/release-name": release}}}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, export, profile,
		claim("wiki-release-data", "wiki-release"),
		// A sibling whose name begins with this app's: not this app's volume.
		claim("wiki-pro-release-data", "wiki-pro-release"),
	).Build()
	r := &TenantExportReconciler{Client: c, Scheme: scheme, Reconciler: &TenantReconciler{Client: c, Scheme: scheme}}
	enc := backup.Encryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, PassphraseSecret: "p", PassphraseKey: "passphrase"}

	units, err := r.captureUnits(context.Background(), tenant, "wiki", profile, export, enc)
	if err != nil {
		t.Fatal(err)
	}
	got := unitArtefacts(units)
	want := []gentianov1alpha1.BundleArtefact{
		{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"},
		{Kind: bundle.ArtefactPostgresOwned, Name: "demo_wiki", Path: "postgres/demo_wiki.owned.tar.gz"},
		{Kind: bundle.ArtefactS3, Name: "demo-wiki", Path: "s3/demo-wiki.tar.gz"},
		{Kind: bundle.ArtefactVolume, Name: "wiki-release-data", Path: "volumes/wiki-release-data.tar.gz", Release: "wiki-release"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured = %+v\nwant %+v", got, want)
	}
	// Every unit writes where it says it does.
	for _, u := range units {
		upload := u.Job.Spec.Template.Spec.Containers[0].Args[0]
		if !strings.Contains(upload, u.Path+backup.EncryptedSuffix) {
			t.Errorf("%s: the Job does not upload to %s", u.Kind, u.Path)
		}
	}

	// The manifest, from what the status recorded.
	export.Status.Apps = []gentianov1alpha1.AppExportStatus{
		{Name: "wiki", ChartVersion: "2.0.0", Digest: "sha256:aaa", QuiesceMode: "scaleDown", Artefacts: got, Stores: unitKinds(units)},
		{Name: backupTenantComponent, Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactIdentity, Name: "demo", Path: backup.IdentityArtefact}}},
	}
	m := r.buildManifest(export, tenant)
	if m.SchemaVersion != 2 || !m.NamesArtefacts() {
		t.Fatalf("schemaVersion = %d", m.SchemaVersion)
	}
	if len(m.Apps) != 1 || m.Apps[0].Name != "wiki" {
		t.Fatalf("apps = %+v, want wiki alone: the tenant-wide captures are not an app", m.Apps)
	}
	app := m.Apps[0]
	if app.Digest != "sha256:aaa" || app.ChartVersion != "2.0.0" || app.DatabaseEngine != "postgresql" || app.QuiesceMode != "scaleDown" {
		t.Errorf("app = %+v", app)
	}
	if !reflect.DeepEqual(app.Releases, []string{"wiki-release", "tenant-demo-wiki"}) {
		t.Errorf("releases = %v", app.Releases)
	}
	if len(app.Stores) != len(want) {
		t.Fatalf("stores = %+v", app.Stores)
	}
	for i, s := range app.Stores {
		if s.Kind != want[i].Kind || s.Name != want[i].Name || s.Path != want[i].Path || s.Release != want[i].Release {
			t.Errorf("store %d = %+v, want %+v", i, s, want[i])
		}
	}
	if m.Identity == nil || m.Identity.Path != "identity/realm.tar.gz" || m.Shell == nil || m.Shell.Path != "postgres/demo_shell.pgc" {
		t.Errorf("identity = %+v, shell = %+v", m.Identity, m.Shell)
	}

	// And a restore of that manifest into the same tenant plans exactly what
	// was captured: the two ends of the format agree.
	plan, err := planRestore(m, tenant, nil, false, liveFrom(tenant,
		map[string]*gentianov1alpha1.ComponentProfile{"wiki": profile},
		map[string][]string{"wiki": {"wiki-release-data"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.apps) != 1 || len(plan.apps[0].artefacts) != len(want) || len(plan.notRestored) != 0 {
		t.Fatalf("plan = %+v, notRestored = %+v", plan.apps, plan.notRestored)
	}
	for i, a := range plan.apps[0].artefacts {
		if a.Path != want[i].Path || a.Target == "" {
			t.Errorf("planned %d = %+v", i, a)
		}
	}
}

// An export captures installed apps only. What an uninstalled app left
// behind is in no bundle taken afterwards, and deleting the tenant destroys
// it. The export does not pretend otherwise: its result and the bundle's
// manifest name the apps on the record of what was provisioned that are not
// installed -- and the desktop's database of the tenant whose desktop is on
// the kernel's PostgreSQL, which no export captures.
func TestAnExportSaysWhatItDoesNotHold(t *testing.T) {
	scheme := deleteGapsScheme()
	tenant := planTenant("demo", "wiki", "drive")
	record := backup.NewProvisionedRecord("demo")
	for app, made := range map[string]backup.Provisioned{
		"wiki":              {DatabaseEngine: gentianov1alpha1.DatabaseEnginePostgreSQL, Database: "demo_wiki"},
		"notes":             {DatabaseEngine: gentianov1alpha1.DatabaseEngineMariaDB, Database: "demo_notes"},
		"crm":               {Bucket: "demo-crm"},
		backup.DesktopStore: {DatabaseEngine: gentianov1alpha1.DatabaseEnginePostgreSQL, Database: "demo_shell"},
	} {
		if _, err := backup.RecordProvisioned(record, app, made); err != nil {
			t.Fatal(err)
		}
	}
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Spec:       gentianov1alpha1.TenantExportSpec{Apps: []string{"wiki"}},
		Status: gentianov1alpha1.TenantExportStatus{
			Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, export, record).Build()
	r := &TenantExportReconciler{Client: c, Scheme: scheme, Reconciler: &TenantReconciler{Client: c, Scheme: scheme}}

	got, err := r.notIncluded(context.Background(), tenant, export)
	if err != nil {
		t.Fatal(err)
	}
	said := strings.Join(got, "\n")
	if !strings.Contains(said, "uninstalled apps left behind (crm, notes,") {
		t.Errorf("the retained apps are not named:\n%s", said)
	}
	if strings.Contains(said, backup.DesktopStore+",") || strings.Contains(said, "(wiki") {
		t.Errorf("an installed app or the desktop's own store is named as left behind:\n%s", said)
	}
	if !strings.Contains(said, "not asked for (drive)") {
		t.Errorf("the installed app the export was not asked for is not named:\n%s", said)
	}
	if strings.Contains(said, "kernel's PostgreSQL") {
		t.Errorf("an ordinary tenant's desktop is said not to be captured:\n%s", said)
	}

	// The manifest carries it, and has the desktop's database.
	export.Status.NotIncluded = got
	m := r.buildManifest(export, tenant)
	if len(m.NotIncluded) != len(got) || m.Shell == nil {
		t.Errorf("manifest: notIncluded = %v, shell = %v", m.NotIncluded, m.Shell)
	}

	// The tenant that adopts the kernel realm: no desktop unit, no desktop
	// entry in the manifest, and said.
	r.Reconciler.KernelRealm = "kernel"
	platform := planTenant("platform")
	platform.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	for _, u := range r.tenantWideUnits(platform, export, backup.Encryption{}) {
		if u.Kind == bundle.ArtefactPostgres {
			t.Errorf("a desktop database is dumped for the tenant whose desktop is on the kernel's PostgreSQL: %s", u.JobName)
		}
	}
	got, err = r.notIncluded(context.Background(), platform, &gentianov1alpha1.TenantExport{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "kernel's PostgreSQL") {
		t.Errorf("notIncluded = %v", got)
	}
	export.Status.NotIncluded = got
	if m := r.buildManifest(export, platform); m.Shell != nil {
		t.Error("the manifest names a desktop database that was not captured")
	}

	// A restore repeats what its bundle says it does not hold.
	restore := &gentianov1alpha1.TenantRestore{}
	recordPlan(restore, &restorePlan{notIncluded: []string{"the data uninstalled apps left behind (crm)"}})
	if notes := strings.Join(restore.Status.Notes, "\n"); !strings.Contains(notes, "The bundle says it does not hold the data uninstalled apps left behind (crm).") {
		t.Errorf("the restore's notes do not repeat it:\n%s", notes)
	}
}
