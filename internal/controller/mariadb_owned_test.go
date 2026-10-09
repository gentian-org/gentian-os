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

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

func scriptOf(job *batchv1.Job, container string) string {
	spec := job.Spec.Template.Spec
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		if c.Name == container {
			return strings.Join(append(append([]string{}, c.Command...), c.Args...), "\n")
		}
	}
	return ""
}

// What a MariaDB app's user is granted is what the inventory says, whether
// or not the app creates databases of its own; and on the shared server
// that is never anything on *.*.
func TestMariaDBProvisioningGrantsWhatTheInventorySays(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	for _, dynamic := range []bool{false, true} {
		script := scriptOf(makeMariaDBSetupJob(tenant, "shop-ce", "pw", dynamic), "provision-db")
		if !strings.Contains(script, backup.MariaDBSetupScript("demo_shop_ce", "demo_shop_ce", dynamic)) {
			t.Fatalf("dynamic=%v: the setup Job does not run the inventory's script:\n%s", dynamic, script)
		}
		if strings.Contains(script, "*.*") {
			t.Errorf("dynamic=%v: the setup Job names *.*", dynamic)
		}
		for _, grant := range backup.MariaDBGrants("demo_shop_ce", "demo_shop_ce", dynamic) {
			if !strings.Contains(script, grant+";") {
				t.Errorf("dynamic=%v: the setup Job does not issue %s", dynamic, grant)
			}
		}
		if got := strings.Contains(script, "GRANT ALL PRIVILEGES ON `demo\\_shop\\_ce\\_%`.* TO 'demo_shop_ce'@'%';"); got != dynamic {
			t.Errorf("dynamic=%v: grant on the prefix present = %v", dynamic, got)
		}
		if !strings.Contains(script, "REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'demo_shop_ce'@'%';") {
			t.Errorf("dynamic=%v: the setup Job cannot take away what an earlier grant gave", dynamic)
		}
	}
}

// An export of a MariaDB app captures the provisioned database and the
// archive of the others that are the app's; a restore puts both back under
// the names of the tenant restored into; and the Job that purges the app and
// the one that deletes its tenant drop by the same question.
func TestMariaDBExportRestoreAndTeardownCoverEveryDatabaseOfTheApp(t *testing.T) {
	scheme := deleteGapsScheme()
	tenant := planTenant("demo", "shop")
	profile := chartProfile("shop", "2.0.0", gentianov1alpha1.DatabaseEngineMariaDB, false)
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantExportStatus{
			Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, export, profile).Build()
	r := &TenantExportReconciler{Client: c, Scheme: scheme, Reconciler: &TenantReconciler{Client: c, Scheme: scheme}}
	enc := backup.Encryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, PassphraseSecret: "p", PassphraseKey: "passphrase"}

	units, err := r.captureUnits(context.Background(), tenant, "shop", profile, export, enc)
	if err != nil {
		t.Fatal(err)
	}
	want := []gentianov1alpha1.BundleArtefact{
		{Kind: bundle.ArtefactMariaDB, Name: "demo_shop", Path: "mariadb/demo_shop.sql.gz"},
		{Kind: bundle.ArtefactMariaDBOwned, Name: "demo_shop", Path: "mariadb/demo_shop.owned.tar.gz"},
	}
	if got := unitArtefacts(units); !reflect.DeepEqual(got, want) {
		t.Fatalf("captured = %+v\nwant %+v", got, want)
	}
	if units[0].JobName == units[1].JobName {
		t.Fatalf("the two captures share a Job name %q", units[0].JobName)
	}
	export.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: "shop", ChartVersion: "2.0.0", Artefacts: want}}
	if m := r.buildManifest(export, tenant); len(m.Apps) != 1 || m.Apps[0].DatabaseEngine != "mariadb" || len(m.Apps[0].Stores) != 2 ||
		m.Apps[0].Stores[1].Kind != "mariadbOwned" || m.SchemaVersion != 3 {
		t.Errorf("manifest = %+v", m)
	}
	// The question each act asks, of this app's database and user: under
	// its prefix, and not what another account holds rights on.
	question := "WHERE BINARY s.schema_name LIKE BINARY 'demo\\_shop\\_%'\n   AND NOT EXISTS (SELECT 1 FROM mysql.db g WHERE NOT (g.User = 'demo_shop' AND g.Host = '%')"
	if dump := scriptOf(units[1].Job, "mariadb-dump-owned"); !strings.Contains(dump, question) {
		t.Errorf("the export does not ask which databases are the app's:\n%s", dump)
	}

	// Restore, into a tenant of another name.
	manifest := &backup.Manifest{SchemaVersion: 2, Tenant: "old", Apps: []backup.ManifestApp{
		{Name: "shop", ChartVersion: "2.0.0", DatabaseEngine: "mariadb", Stores: []backup.ManifestStore{
			{Kind: bundle.ArtefactMariaDB, Name: "old_shop", Path: "mariadb/old_shop.sql.gz"},
			{Kind: bundle.ArtefactMariaDBOwned, Name: "old_shop", Path: "mariadb/old_shop.owned.tar.gz"},
		}},
	}}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"shop": profile}
	plan, err := planRestore(manifest, tenant, nil, false, targetOf(tenant), liveFrom(tenant, profiles, nil))
	if err != nil {
		t.Fatal(err)
	}
	planned := []gentianov1alpha1.BundleArtefact{
		{Kind: bundle.ArtefactMariaDB, Name: "old_shop", Path: "mariadb/old_shop.sql.gz", Target: "demo_shop"},
		{Kind: bundle.ArtefactMariaDBOwned, Name: "old_shop", Path: "mariadb/old_shop.owned.tar.gz", Target: "demo_shop"},
	}
	if len(plan.apps) != 1 || len(plan.notRestored) != 0 || !reflect.DeepEqual(plan.apps[0].artefacts, planned) {
		t.Fatalf("plan = %+v, not restored %+v", plan.apps, plan.notRestored)
	}
	restore := &gentianov1alpha1.TenantRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "back", Namespace: "tenant-demo"},
		Status: gentianov1alpha1.TenantRestoreStatus{
			Bundle: &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"},
			Apps:   []gentianov1alpha1.AppExportStatus{{Name: "shop", Artefacts: planned}},
		},
	}
	rr := &TenantRestoreReconciler{Client: c, Scheme: scheme, Tenant: &TenantReconciler{Client: c, Scheme: scheme}}
	back, err := rr.restoreUnits(context.Background(), tenant, "shop", restore, backup.Decryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, SecretName: "p", SecretKey: "passphrase"})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[1].Kind != bundle.ArtefactMariaDBOwned || back[0].JobName == back[1].JobName {
		t.Fatalf("restore units = %+v", back)
	}
	owned := scriptOf(back[1].Job, "mariadb-restore-owned")
	for _, want := range []string{"SRC='old_shop'", "DB='demo_shop'", question} {
		if !strings.Contains(owned, want) {
			t.Errorf("the restore of the app's other databases lacks %q:\n%s", want, owned)
		}
	}

	// A platform that does not know a kind of artefact does not restore the
	// app without it: it refuses the app and says why. That is what a
	// reader of format 2 from before this kind does with mariadbOwned.
	manifest.Apps[0].Stores[1].Kind = "somethingNewer"
	if _, err = planRestore(manifest, tenant, nil, false, targetOf(tenant), liveFrom(tenant, profiles, nil)); err == nil ||
		!strings.Contains(err.Error(), `shop has an artefact of kind "somethingNewer" in the bundle, which this platform does not know how to restore`) {
		t.Errorf("an unknown kind of artefact was not refused: %v", err)
	}

	// Purge and the deletion of the tenant: one Job, one script.
	purge := backup.StoreDestroyJobs(tenant, "shop", backup.ProfileStores(profile), backup.DestroyWithinARequest)[backup.KindDatabase]
	deletion := makeMariaDBDeleteJob(tenant, "shop")
	for name, job := range map[string]*batchv1.Job{"purge": purge, "tenant deletion": deletion} {
		script := scriptOf(job, "delete-db")
		for _, want := range []string{question, "DROP DATABASE IF EXISTS `demo_shop`;", "DROP USER IF EXISTS 'demo_shop'@'%';", "are still there"} {
			if !strings.Contains(script, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	if scriptOf(purge, "delete-db") != scriptOf(deletion, "delete-db") {
		t.Error("the purge of an app and the deletion of its tenant drop by different scripts")
	}
}
