/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// adminSecret is a credential a chart puts beside its service, and nowhere
// else: the fixture every placement test starts from.
type adminSecret struct {
	name, namespace string
	// chart is the template that makes it, read to hold this table to the
	// charts: a Secret that is renamed, or made by another chart, fails here.
	chart string
	data  map[string]string
}

func adminSecrets() []adminSecret {
	return []adminSecret{
		{backup.PostgresAdminSecret, postgresNamespace, "kernel/data/tenant-postgres/templates/credentials.yaml",
			map[string]string{"host": "pg", "port": "5432", "username": "postgres", "password": "PG-ADMIN-PASSWORD"}},
		{backup.MariaDBAdminSecret, mariadbNamespace, "kernel/services/infra-mariadb/manifests/templates/externalsecret.yaml",
			map[string]string{"host": "maria", "port": "3306", "username": "root", "password": "MARIA-ADMIN-PASSWORD"}},
		{backup.KeycloakAdminSecret, identityNamespace, "kernel/services/keycloak-idp/manifests/templates/externalsecret-admin.yaml",
			map[string]string{"url": "http://keycloak", "username": "admin", "password": "KC-ADMIN-PASSWORD"}},
		{backup.MinIOAdminSecret, s3Namespace, "kernel/services/infra-minio/manifests/templates/externalsecret.yaml",
			map[string]string{"endpoint": "http://minio:9000", "accessKey": "minio-root", "secretKey": "MINIO-ROOT-SECRET"}},
	}
}

// The fixture is the charts': each administrator Secret is made by the chart
// of its own service, which the layout installs into that service's
// namespace and no other.
func TestTheAdministratorSecretsAreMadeBesideTheirServices(t *testing.T) {
	seen := map[string]string{}
	for _, s := range adminSecrets() {
		raw, err := os.ReadFile(filepath.Join("..", "..", s.chart))
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if !bytes.Contains(raw, []byte("name: "+s.name+"\n")) {
			t.Errorf("%s does not make a Secret named %s", s.chart, s.name)
		}
		if other, dup := seen[s.namespace]; dup {
			t.Errorf("%s and %s are both placed in %s: this test needs each service in a namespace of its own", s.name, other, s.namespace)
		}
		seen[s.namespace] = s.name
	}
	for fn, ns := range map[string]string{"postgresql": postgresNamespace, "mariadb": mariadbNamespace, "s3": s3Namespace} {
		if ns != layout.System(fn) {
			t.Errorf("%s is placed in %s", fn, ns)
		}
	}
	if identityNamespace != layout.Namespace(layout.Authentication) {
		t.Errorf("the identity provider is placed in %s", identityNamespace)
	}
}

// placementWorld is tenant acme with wiki (PostgreSQL, a bucket, a volume)
// and shop (MariaDB), on a cluster whose Secrets are where the charts put
// them.
type placementWorld struct {
	c       client.Client
	tenant  *gentianov1alpha1.Tenant
	export  *gentianov1alpha1.TenantExport
	restore *gentianov1alpha1.TenantRestore
	er      *TenantExportReconciler
	rr      *TenantRestoreReconciler
}

const externalDestinationSecret = "backup-destination-acme"

func newPlacementWorld(t *testing.T, external bool) *placementWorld {
	t.Helper()
	scheme := deleteGapsScheme()
	tenant := planTenant("acme", "wiki", "shop")
	ref := gentianov1alpha1.BundleRef{Bucket: "acme-gentian-backup", Prefix: "export-1"}
	if external {
		ref.Endpoint, ref.CredentialSecret = "https://s3.example.org", externalDestinationSecret
	}
	export := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "export-1", Namespace: "tenant-acme"}}
	export.Status.Bundle = ref.DeepCopy()
	restore := &gentianov1alpha1.TenantRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-1", Namespace: "tenant-acme"},
		Spec: gentianov1alpha1.TenantRestoreSpec{Decryption: &gentianov1alpha1.RestoreDecryption{
			PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "my-key", Key: "passphrase"},
			IdentitySecretRef:   &gentianov1alpha1.SecretKeyRef{Name: "my-key", Key: "identity"},
		}},
	}
	restore.Status.Bundle = ref.DeepCopy()
	restore.Status.Apps = []gentianov1alpha1.AppExportStatus{
		{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactPostgres, Name: "acme_wiki", Path: "postgres/acme_wiki.pgc", Target: "acme_wiki"},
			{Kind: bundle.ArtefactPostgresOwned, Name: "acme_wiki", Path: "postgres/acme_wiki.owned.tar.gz", Target: "acme_wiki"},
			{Kind: bundle.ArtefactS3, Name: "acme-wiki", Path: "s3/acme-wiki.tar.gz", Target: "acme-wiki"},
			{Kind: bundle.ArtefactVolume, Name: "wiki-data", Path: "volumes/wiki-data.tar.gz", Target: "wiki-data"},
		}},
		{Name: "shop", Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactMariaDB, Name: "acme_shop", Path: "mariadb/acme_shop.sql.gz", Target: "acme_shop"},
			{Kind: bundle.ArtefactMariaDBOwned, Name: "acme_shop", Path: "mariadb/acme_shop.owned.tar.gz", Target: "acme_shop"},
		}},
		{Name: backupTenantComponent, Artefacts: []gentianov1alpha1.BundleArtefact{
			{Kind: bundle.ArtefactIdentity, Name: "acme", Path: backup.IdentityArtefact, Target: "acme"},
			{Kind: bundle.ArtefactPostgres, Name: "acme_portal_shell", Path: "postgres/acme_portal_shell.pgc", Target: "acme_portal_shell"},
		}},
	}

	objects := []client.Object{
		tenant, export, restore,
		chartProfile("wiki", "1.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true),
		chartProfile("shop", "1.0.0", gentianov1alpha1.DatabaseEngineMariaDB, false),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "wiki-data", Namespace: "tenant-acme",
			Labels: map[string]string{"gentianos.io/app": "wiki"}}},
		// What a person who asks for a restore puts in the tenant's namespace.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-key", Namespace: "tenant-acme"},
			Data: map[string][]byte{"passphrase": []byte("THE-PASSPHRASE"), "identity": []byte("AGE-SECRET-KEY-1")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: externalDestinationSecret, Namespace: s3Namespace},
			Data: map[string][]byte{backup.DestinationAccessKeyField: []byte("ext-key"), backup.DestinationSecretKeyField: []byte("EXT-SECRET")}},
	}
	for _, s := range adminSecrets() {
		data := map[string][]byte{}
		for k, v := range s.data {
			data[k] = []byte(v)
		}
		objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace}, Data: data})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&gentianov1alpha1.TenantRestore{}, &gentianov1alpha1.TenantExport{}).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme}
	er := &TenantExportReconciler{Client: c, Scheme: scheme, Reconciler: tr}
	return &placementWorld{c: c, tenant: tenant, export: export, restore: restore, er: er,
		rr: &TenantRestoreReconciler{Client: c, Scheme: scheme, Tenant: tr, Reconciler: er}}
}

// exportUnits are every unit an export of the tenant makes, by the
// constructors the reconciler runs, with what it stages for them.
func (w *placementWorld) exportUnits(t *testing.T, mode gentianov1alpha1.ExportEncryptionMode) []captureUnit {
	t.Helper()
	ctx := context.Background()
	w.export.Spec.Encryption = &gentianov1alpha1.ExportEncryption{Mode: mode}
	if mode == gentianov1alpha1.ExportEncryptionPassphrase {
		w.export.Spec.Encryption.PassphraseSecretRef = &gentianov1alpha1.SecretKeyRef{Name: "my-key"}
	} else {
		w.export.Spec.Encryption.Recipients = []string{"age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"}
	}
	enc, err := w.er.resolveEncryption(ctx, w.export)
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	var units []captureUnit
	for _, app := range []string{"wiki", "shop"} {
		profile, err := resolveProfile(ctx, w.c, w.tenant, app)
		if err != nil {
			t.Fatal(err)
		}
		more, err := w.er.captureUnits(ctx, w.tenant, app, profile, w.export, enc)
		if err != nil {
			t.Fatalf("captureUnits(%s): %v", app, err)
		}
		units = append(units, more...)
	}
	units = append(units, w.er.tenantWideUnits(w.tenant, w.export, enc)...)
	manifest, err := w.er.manifestUnit(w.export, w.tenant, enc)
	if err != nil {
		t.Fatal(err)
	}
	units = append(units, manifest)
	if err := w.er.stageFor(ctx, w.export, units, enc); err != nil {
		t.Fatalf("stageFor: %v", err)
	}
	return units
}

// restoreUnits are every unit a restore into the tenant makes.
func (w *placementWorld) restoreUnits(t *testing.T, mode gentianov1alpha1.ExportEncryptionMode) []captureUnit {
	t.Helper()
	ctx := context.Background()
	d, err := w.rr.resolveDecryption(ctx, w.restore, mode)
	if err != nil {
		t.Fatalf("resolveDecryption: %v", err)
	}
	var units []captureUnit
	for _, app := range []string{"wiki", "shop"} {
		more, err := w.rr.restoreUnits(ctx, w.tenant, app, w.restore, d)
		if err != nil {
			t.Fatalf("restoreUnits(%s): %v", app, err)
		}
		units = append(units, more...)
	}
	units = append(units, w.rr.tenantWideRestoreUnits(w.tenant, w.restore, d)...)
	if err := w.rr.stageFor(ctx, w.restore, units, d); err != nil {
		t.Fatalf("stageFor: %v", err)
	}
	return units
}

// podReferences are the Secrets and ConfigMaps a pod cannot start without,
// with the key where one is named.
type podReference struct {
	kind, name, key, where string
}

func podReferences(spec corev1.PodSpec) []podReference {
	var refs []podReference
	containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for _, c := range containers {
		for _, env := range c.Env {
			if env.ValueFrom == nil {
				continue
			}
			if r := env.ValueFrom.SecretKeyRef; r != nil && (r.Optional == nil || !*r.Optional) {
				refs = append(refs, podReference{"Secret", r.Name, r.Key, c.Name + " env " + env.Name})
			}
			if r := env.ValueFrom.ConfigMapKeyRef; r != nil && (r.Optional == nil || !*r.Optional) {
				refs = append(refs, podReference{"ConfigMap", r.Name, r.Key, c.Name + " env " + env.Name})
			}
		}
		for _, from := range c.EnvFrom {
			if r := from.SecretRef; r != nil && (r.Optional == nil || !*r.Optional) {
				refs = append(refs, podReference{"Secret", r.Name, "", c.Name + " envFrom"})
			}
			if r := from.ConfigMapRef; r != nil && (r.Optional == nil || !*r.Optional) {
				refs = append(refs, podReference{"ConfigMap", r.Name, "", c.Name + " envFrom"})
			}
		}
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil && (v.Secret.Optional == nil || !*v.Secret.Optional) {
			refs = append(refs, podReference{"Secret", v.Secret.SecretName, "", "volume " + v.Name})
		}
		if v.ConfigMap != nil && (v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional) {
			refs = append(refs, podReference{"ConfigMap", v.ConfigMap.Name, "", "volume " + v.Name})
		}
	}
	return refs
}

// assertUnitsCanStart holds every unit to the one thing that decides whether
// its pod is ever created: each Secret and ConfigMap it references is in the
// namespace the unit runs in, with the key it reads.
func assertUnitsCanStart(t *testing.T, c client.Client, units []captureUnit) {
	t.Helper()
	ctx := context.Background()
	for _, unit := range units {
		ns := unit.Job.Namespace
		for _, ref := range podReferences(unit.Job.Spec.Template.Spec) {
			var data map[string][]byte
			var err error
			if ref.kind == "Secret" {
				secret := &corev1.Secret{}
				err = c.Get(ctx, types.NamespacedName{Name: ref.name, Namespace: ns}, secret)
				data = secret.Data
			} else {
				cm := &corev1.ConfigMap{}
				err = c.Get(ctx, types.NamespacedName{Name: ref.name, Namespace: ns}, cm)
				data = map[string][]byte{}
				for k, v := range cm.Data {
					data[k] = []byte(v)
				}
			}
			if err != nil {
				t.Errorf("%s (%s) runs in %s and reads %s %s (%s), which is not there: its pod is never created",
					unit.JobName, unit.Kind, ns, ref.kind, ref.name, ref.where)
				continue
			}
			if ref.key != "" && len(data[ref.key]) == 0 {
				t.Errorf("%s (%s) reads key %q of %s %s in %s (%s), which it does not have",
					unit.JobName, unit.Kind, ref.key, ref.kind, ref.name, ns, ref.where)
			}
		}
	}
}

// assertNoAdminCredentialWasCopied: an administrator's password is in the
// Secret its chart made and in no other object of the cluster.
func assertNoAdminCredentialWasCopied(t *testing.T, c client.Client) {
	t.Helper()
	secrets := &corev1.SecretList{}
	if err := c.List(context.Background(), secrets); err != nil {
		t.Fatal(err)
	}
	for _, admin := range adminSecrets() {
		if admin.name == backup.MinIOAdminSecret {
			// The credential a bundle on the platform's own storage is written
			// with: it is what is staged.
			continue
		}
		for _, s := range secrets.Items {
			if s.Name == admin.name && s.Namespace == admin.namespace {
				continue
			}
			for k, v := range s.Data {
				if string(v) == admin.data["password"] {
					t.Errorf("the password of %s is copied into %s/%s (key %s)", admin.name, s.Namespace, s.Name, k)
				}
			}
		}
	}
}

func unitsByNamespace(units []captureUnit) map[string][]string {
	out := map[string][]string{}
	for _, u := range units {
		out[u.Job.Namespace] = append(out[u.Job.Namespace], u.Kind)
	}
	return out
}

// The class of mistake that stopped every backup: a unit that runs in one
// namespace and reads a Secret that exists in another. Its pod is never
// created, so it never fails, and nothing but a cluster shows it. Every unit
// an export and a restore make -- by the reconcilers' own constructors, for
// each way of encrypting a bundle and each place a bundle can be -- is held
// here to: what its pod reads is where it runs.
func TestEveryUnitFindsWhatItsPodReadsWhereItRuns(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, mode := range []gentianov1alpha1.ExportEncryptionMode{
			gentianov1alpha1.ExportEncryptionRecipient, gentianov1alpha1.ExportEncryptionPassphrase,
		} {
			t.Run(fmt.Sprintf("external=%v/%s", external, mode), func(t *testing.T) {
				w := newPlacementWorld(t, external)

				exports := w.exportUnits(t, mode)
				// Every kind there is, so that a kind added without a place is seen.
				wantExport := map[string][]string{
					postgresNamespace: {bundle.ArtefactPostgres, bundle.ArtefactPostgresOwned, bundle.ArtefactPostgres},
					mariadbNamespace:  {bundle.ArtefactMariaDB, bundle.ArtefactMariaDBOwned},
					s3Namespace:       {bundle.ArtefactS3, "manifest"},
					"tenant-acme":     {bundle.ArtefactVolume},
					identityNamespace: {bundle.ArtefactIdentity},
				}
				if got := unitsByNamespace(exports); fmt.Sprint(got) != fmt.Sprint(wantExport) {
					t.Errorf("export units run in\n  %v\nwant\n  %v", got, wantExport)
				}
				assertUnitsCanStart(t, w.c, exports)

				restores := w.restoreUnits(t, mode)
				wantRestore := map[string][]string{
					postgresNamespace: {bundle.ArtefactPostgres, bundle.ArtefactPostgresOwned, bundle.ArtefactPostgres},
					mariadbNamespace:  {bundle.ArtefactMariaDB, bundle.ArtefactMariaDBOwned},
					s3Namespace:       {bundle.ArtefactS3},
					"tenant-acme":     {bundle.ArtefactVolume},
					identityNamespace: {bundle.ArtefactIdentity},
				}
				if got := unitsByNamespace(restores); fmt.Sprint(got) != fmt.Sprint(wantRestore) {
					t.Errorf("restore units run in\n  %v\nwant\n  %v", got, wantRestore)
				}
				assertUnitsCanStart(t, w.c, restores)

				assertNoAdminCredentialWasCopied(t, w.c)

				// What is staged is the credential of where the bundle is.
				staged := &corev1.Secret{}
				if err := w.c.Get(context.Background(), types.NamespacedName{
					Name: stagedSecretName("acme", "export-1"), Namespace: postgresNamespace}, staged); err != nil {
					t.Fatal(err)
				}
				if external {
					if string(staged.Data[backup.DestinationSecretKeyField]) != "EXT-SECRET" || len(staged.Data["secretKey"]) != 0 && string(staged.Data["secretKey"]) == "MINIO-ROOT-SECRET" {
						t.Errorf("an external bundle's units were staged %v", keysOf(staged.Data))
					}
				} else if string(staged.Data["secretKey"]) != "MINIO-ROOT-SECRET" {
					t.Errorf("a platform bundle's units were staged %v", keysOf(staged.Data))
				}

				// And at the end of each run, every copy is gone from every
				// namespace: the passphrase and the keys are held for a run.
				if err := w.er.discardPassphrase(context.Background(), w.export); err != nil {
					t.Fatal(err)
				}
				if err := w.er.discardStagedSecrets(context.Background(), w.export); err != nil {
					t.Fatal(err)
				}
				if err := w.rr.discardStagedRestoreSecrets(context.Background(), w.restore); err != nil {
					t.Fatal(err)
				}
				left := &corev1.SecretList{}
				if err := w.c.List(context.Background(), left); err != nil {
					t.Fatal(err)
				}
				for _, s := range left.Items {
					if s.Labels[backup.ExportLabel] != "" {
						t.Errorf("a staged Secret outlived its run: %s/%s", s.Namespace, s.Name)
					}
				}
			})
		}
	}
}

func keysOf(data map[string][]byte) []string {
	var keys []string
	for k := range data {
		keys = append(keys, k)
	}
	return keys
}

// A run's objects are made in namespaces every tenant shares, and a run's
// name is unique in its tenant only. Two tenants' exports of one name -- two
// schedules called "nightly" firing in the same minute -- must not share a
// Job or a Secret: the second used to find the first's finished Job under
// its own name and record the capture as made, and its passphrase replaced
// the first's.
func TestTwoTenantsRunsOfOneNameShareNothing(t *testing.T) {
	names := map[string]string{}
	for _, tenant := range []string{"acme", "globex"} {
		for what, name := range map[string]string{
			"a Job":                    exportJobName(tenant, "nightly-20261008-0300", "wiki", "pg"),
			"the staged copy":          stagedSecretName(tenant, "nightly-20261008-0300"),
			"a restore's staged copy":  restoreStagedSecretName(tenant, "nightly-20261008-0300"),
			"the passphrase":           passphraseSecretName(tenant, "nightly-20261008-0300"),
			"the decryption key":       stagedDecryptionSecretName(tenant, "nightly-20261008-0300"),
			"the destination's keys":   backup.ExportCredentialSecretName(tenant, "nightly-20261008-0300"),
			"the bundle's cleanup Job": bundleDeleteJobName(tenant, "nightly-20261008-0300"),
			"a long Job":               exportJobName(tenant, "nightly-20261008-0300", strings.Repeat("nextcloud-base-edition", 3), "pg"),
		} {
			if other, taken := names[name]; taken {
				t.Errorf("%s of %s is named %s, as is %s", what, tenant, name, other)
			}
			names[name] = what + " of " + tenant
			if len(name) > 63 {
				t.Errorf("%s is %d characters", name, len(name))
			}
		}
	}

	// And where two names do meet -- "a-b" with "c", "a" with "b-c" -- the
	// object found is not taken for this run's.
	w := newPlacementWorld(t, false)
	units := w.exportUnits(t, gentianov1alpha1.ExportEncryptionRecipient)
	var pg captureUnit
	for _, u := range units {
		if u.Kind == bundle.ArtefactPostgres && strings.HasSuffix(u.JobName, "-wiki-pg") {
			pg = u
		}
	}
	foreign := pg.Job.DeepCopy()
	foreign.Labels[tenantLabel] = "globex"
	foreign.Spec.Template.Labels[tenantLabel] = "globex"
	if err := w.c.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	foreign.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := w.c.Status().Update(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	done, err := w.er.ensureCaptureJob(context.Background(), w.export, pg)
	if err != nil {
		t.Fatal(err)
	}
	entry := appStatus(&w.export.Status.Apps, "wiki")
	if done || len(entry.CompletedUnits) != 0 {
		t.Fatal("another tenant's finished Job was recorded as this export's capture")
	}
	if entry.Attempts == 0 || !strings.Contains(entry.LastFailure, "belongs to another run") {
		t.Errorf("the name another run holds is not counted or not said: %d, %q", entry.Attempts, entry.LastFailure)
	}

	// A staged Secret another run holds is not overwritten either.
	other := runSecret(stagedSecretName("acme", "export-1"), mariadbNamespace, "globex", "export-1",
		map[string][]byte{"accessKey": []byte("theirs")})
	_ = w.c.Delete(context.Background(), other.DeepCopy())
	if err := w.c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := w.er.ensureStagedSecret(context.Background(), w.export, mariadbNamespace, backup.Encryption{}); err == nil ||
		!strings.Contains(err.Error(), "belongs to another run") {
		t.Errorf("another run's staged Secret was taken over: %v", err)
	}
}
