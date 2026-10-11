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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// An app's own secrets through a backup and a restore.
//
// What is shown here is shown without a cluster: the vault is a map with the
// vault's write-once rule, the object store keeps what the operator hands it,
// and the secrets operator and the Helm provider are played by the test,
// which fills a Secret when its ExternalSecret was told to read again and
// upgrades a release when it was told to look. The operator's part is the
// real code throughout, the encryption included.

// secretVault is a vault: paths hold records, the first write of PutOnce
// stands, and a path that holds nothing is secrets.ErrNotFound.
type secretVault struct {
	mu   sync.Mutex
	data map[string]map[string]string
	// puts are the paths written unconditionally, in order.
	puts []string
}

func newSecretVault() *secretVault { return &secretVault{data: map[string]map[string]string{}} }

func (v *secretVault) PutOnce(_ context.Context, p string, data map[string]string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.data[p]; !ok {
		v.data[p] = data
	}
	return nil
}

func (v *secretVault) Put(_ context.Context, p string, data map[string]string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.data[p] = data
	v.puts = append(v.puts, p)
	return nil
}

func (v *secretVault) Get(_ context.Context, p string) (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	record, ok := v.data[p]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", p, secrets.ErrNotFound)
	}
	return record, nil
}

// purge removes an app's subtree, as a purge of the app does.
func (v *secretVault) purge(tenant, app string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for p := range v.data {
		if strings.HasPrefix(p, secrets.AppPath(tenant, app)+"/") {
			delete(v.data, p)
		}
	}
}

func (v *secretVault) value(p string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.data[p]["value"]
}

// snapshot is every app's own secret the vault holds, by path. What else an
// install or a restore keeps there -- a bucket's keys, a database's password --
// is not what these tests are about.
func (v *secretVault) snapshot() map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]string{}
	for p, record := range v.data {
		if strings.Contains(p, "/internal/") {
			out[p] = record["value"]
		}
	}
	return out
}

// appEncrypt is what an app does with a secret of its own: it encrypts what
// it stores with a key made from it. appDecrypt reads it back, and fails
// with any other secret.
func appEncrypt(t *testing.T, secret, plain string) []byte {
	t.Helper()
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil)
}

func appDecrypt(secret string, data []byte) (string, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	return string(plain), err
}

// secretsProfile is wiki as an app that has the platform generate secrets
// for it and for one extension.
func secretsProfile() *gentianov1alpha1.ComponentProfile {
	p := chartProfile("wiki", "2.0.0", gentianov1alpha1.DatabaseEnginePostgreSQL, true)
	p.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Generated: []gentianov1alpha1.AppSecret{
		{Name: "encryption_key", ValuePath: "secrets.encryptionKey"},
		{Name: "session_key", ValuePath: "secrets.sessionKey"},
	}}
	p.Spec.Extensions = []gentianov1alpha1.AppSidecarSpec{{
		Name: "search", Chart: gentianov1alpha1.ChartRef{Name: "search", Version: "1.0.0"},
		AppSecrets: []gentianov1alpha1.AppSecret{{Name: "index_key", ValuePath: "indexKey"}},
	}}
	return p
}

const (
	wikiEncryption = "gentian-os/tenants/demo/apps/wiki/internal/encryption_key"
	wikiSession    = "gentian-os/tenants/demo/apps/wiki/internal/session_key"
	wikiIndex      = "gentian-os/tenants/demo/apps/wiki-search/internal/index_key"
)

// secretsWorld is tenant demo with wiki installed, on a cluster whose
// secrets are random, with the objects an installed app's secrets reach it
// through: the ExternalSecret and the Secret it fills, the Helm release that
// is handed a key of that Secret as a value, and the app's Deployment.
type secretsWorld struct {
	*restoreWorld
	vault  *secretVault
	seeder *secrets.Seeder
	export *gentianov1alpha1.TenantExport
	er     *TenantExportReconciler
}

func randomMode(context.Context) (secrets.Mode, error) { return secrets.ModeRandom, nil }

func newSecretsWorld(t *testing.T, mutate func(*gentianov1alpha1.TenantRestore)) *secretsWorld {
	t.Helper()
	ctx := context.Background()
	rw := newRestoreWorld(t, mutate)
	vault := newSecretVault()
	seeder := secrets.NewSeeder(vault, secrets.NewDeriver("a master password", "a salt")).WithMode(randomMode)
	rw.r.Tenant.Seeder = seeder

	profile := &gentianov1alpha1.ComponentProfile{}
	if err := rw.c.Get(ctx, types.NamespacedName{Name: "wiki"}, profile); err != nil {
		t.Fatal(err)
	}
	want := secretsProfile()
	profile.Spec.Secrets, profile.Spec.Extensions = want.Spec.Secrets, want.Spec.Extensions
	if err := rw.c.Update(ctx, profile); err != nil {
		t.Fatal(err)
	}

	es := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"target": map[string]any{"name": "wiki-sensitive-values"},
			"data": []any{
				map[string]any{"secretKey": "internal-encryption_key", "remoteRef": map[string]any{"key": wikiEncryption, "property": "value"}},
				map[string]any{"secretKey": "internal-session_key", "remoteRef": map[string]any{"key": wikiSession, "property": "value"}},
				map[string]any{"secretKey": "db-password", "remoteRef": map[string]any{"key": "gentian-os/tenants/demo/apps/wiki/database", "property": "password"}},
			},
		},
	}}
	es.SetGroupVersionKind(externalSecretGVK)
	es.SetName("wiki-sensitive-values")
	es.SetNamespace("tenant-demo")
	other := es.DeepCopy()
	other.SetName("drive-sensitive-values")
	_ = unstructured.SetNestedField(other.Object, "drive-sensitive-values", "spec", "target", "name")
	_ = unstructured.SetNestedSlice(other.Object, []any{
		map[string]any{"secretKey": "db-password", "remoteRef": map[string]any{"key": "gentian-os/tenants/demo/apps/drive/database", "property": "password"}},
	}, "spec", "data")

	release := func(name, secret, key string) *unstructured.Unstructured {
		r := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"forProvider": map[string]any{"set": []any{
				map[string]any{"name": "some.value", "valueFrom": map[string]any{"secretKeyRef": map[string]any{
					"name": secret, "namespace": "tenant-demo", "key": key}}},
			}}},
			"status": map[string]any{
				"atProvider": map[string]any{"revision": int64(3)},
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}}
		r.SetGroupVersionKind(helmReleaseGVK)
		r.SetName(name)
		return r
	}
	for _, obj := range []client.Object{
		es, other,
		release("demo-wiki", "wiki-sensitive-values", "internal-encryption_key"),
		// Handed the database password only: none of its values changes.
		release("demo-wiki-db", "wiki-sensitive-values", "db-password"),
		release("demo-drive", "drive-sensitive-values", "db-password"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wiki-sensitive-values", Namespace: "tenant-demo"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-demo"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "drive", Namespace: "tenant-demo"}},
	} {
		if err := rw.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Spec: gentianov1alpha1.TenantExportSpec{Encryption: &gentianov1alpha1.ExportEncryption{
			Mode:                gentianov1alpha1.ExportEncryptionPassphrase,
			PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "r1-key", Key: "passphrase"},
		}},
		// The restore's bundle: what this export writes is what it reads.
		Status: gentianov1alpha1.TenantExportStatus{Bundle: rw.restore(t).Spec.Bundle},
	}
	w := &secretsWorld{restoreWorld: rw, vault: vault, seeder: seeder, export: export,
		er: &TenantExportReconciler{Client: rw.c, Reconciler: rw.r.Tenant, Bundles: rw.bundles}}
	w.install(t)
	return w
}

var passphraseEncryption = backup.Encryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase, PassphraseSecret: "p", PassphraseKey: "passphrase"}

// install seeds wiki's secrets as an install does, and has the secrets
// operator fill the app's Secret from them.
func (w *secretsWorld) install(t *testing.T) {
	t.Helper()
	tenant := planTenant("demo", "wiki", "drive")
	if err := w.r.Tenant.seedAppSecrets(context.Background(), tenant, "wiki", secretsProfile()); err != nil {
		t.Fatal(err)
	}
	w.playSecretsOperator(t, true)
}

// backup captures wiki's secrets into the bundle and returns the manifest an
// export would write for it: wiki with a database dump and its secrets.
func (w *secretsWorld) backup(t *testing.T, of string) *backup.Manifest {
	t.Helper()
	entry := gentianov1alpha1.AppExportStatus{Name: "wiki", ChartVersion: "2.0.0",
		Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres, Name: of + "_wiki", Path: backup.PostgresArtefact(of + "_wiki")}}}
	if err := w.er.withAppSecrets(context.Background(), w.export, planTenant("demo", "wiki"), "wiki", secretsProfile(), passphraseEncryption, &entry); err != nil {
		t.Fatal(err)
	}
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{entry}
	m := w.er.buildManifest(w.export, planTenant(of, "wiki"))
	m.Tenant = of
	m.Identity, m.Shell = nil, nil
	return m
}

// playSecretsOperator fills each Secret from the vault for an ExternalSecret
// that was told to read again -- or for every one, at an install.
func (w *secretsWorld) playSecretsOperator(t *testing.T, all bool) {
	t.Helper()
	ctx := context.Background()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(externalSecretGVK.GroupVersion().WithKind(externalSecretGVK.Kind + "List"))
	if err := w.c.List(ctx, list, client.InNamespace("tenant-demo")); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		es := &list.Items[i]
		if !all && es.GetAnnotations()[forceSyncAnnotation] == "" {
			continue
		}
		target, _, _ := unstructured.NestedString(es.Object, "spec", "target", "name")
		secret := &corev1.Secret{}
		if err := w.c.Get(ctx, types.NamespacedName{Name: target, Namespace: "tenant-demo"}, secret); err != nil {
			continue
		}
		secret.Data = map[string][]byte{}
		data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
		for _, raw := range data {
			item := raw.(map[string]any)
			ref := item["remoteRef"].(map[string]any)
			record, _ := w.vault.Get(ctx, ref["key"].(string))
			secret.Data[item["secretKey"].(string)] = []byte(record[ref["property"].(string)])
		}
		if err := w.c.Update(ctx, secret); err != nil {
			t.Fatal(err)
		}
	}
}

// playHelmProvider upgrades each release that was told to look at its
// values again, once.
func (w *secretsWorld) playHelmProvider(t *testing.T) (upgraded []string) {
	t.Helper()
	ctx := context.Background()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(helmReleaseGVK.GroupVersion().WithKind(helmReleaseGVK.Kind + "List"))
	if err := w.c.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		release := &list.Items[i]
		if release.GetAnnotations()[secretsRestoredAnnotation] == "" || releaseRevision(release) != 3 {
			continue
		}
		_ = unstructured.SetNestedField(release.Object, int64(4), "status", "atProvider", "revision")
		if err := w.c.Update(ctx, release); err != nil {
			t.Fatal(err)
		}
		upgraded = append(upgraded, release.GetName())
	}
	return upgraded
}

// runWithOperators is restoreWorld.run with the secrets operator and the
// Helm provider doing their part between passes. It returns the restore and
// every release that was upgraded.
func (w *secretsWorld) runWithOperators(t *testing.T) (*gentianov1alpha1.TenantRestore, []string) {
	t.Helper()
	ctx := context.Background()
	var upgraded []string
	for i := 0; i < 60; i++ {
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		w.playSecretsOperator(t, false)
		upgraded = append(upgraded, w.playHelmProvider(t)...)
		for _, job := range w.jobs(t) {
			if len(job.Status.Conditions) == 0 {
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				if err := w.c.Status().Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := w.restore(t); got.IsTerminal() && i > 0 {
			return got, upgraded
		}
	}
	t.Fatalf("the restore did not end: %+v", w.restore(t).Status)
	return nil, nil
}

func (w *secretsWorld) wikiStatus(t *testing.T) *gentianov1alpha1.AppExportStatus {
	t.Helper()
	got := w.restore(t)
	for i := range got.Status.Apps {
		if got.Status.Apps[i].Name == "wiki" {
			return &got.Status.Apps[i]
		}
	}
	t.Fatalf("no status for wiki: %+v", got.Status.Apps)
	return nil
}

func (w *secretsWorld) restarted(t *testing.T, name string) bool {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := w.c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "tenant-demo"}, d); err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Annotations[restartWorkloadAnnotation] != ""
}

// An export puts an app's own secrets into the bundle as one more artefact
// of the app: every one its profile declares and the vault holds, its
// extensions' too, encrypted by the operator under the bundle's own key. No
// value is in the manifest, in the status or in a Job.
func TestAnExportCarriesAnAppsOwnSecretsEncryptedAndNowhereElse(t *testing.T) {
	w := newSecretsWorld(t, nil)
	values := w.vault.snapshot()
	if len(values) != 3 || values[wikiEncryption] == "" || values[wikiIndex] == "" {
		t.Fatalf("the install seeded %v", values)
	}
	m := w.backup(t, "demo")

	entry := w.export.Status.Apps[0]
	if got := entry.Artefacts[len(entry.Artefacts)-1]; got != (gentianov1alpha1.BundleArtefact{Kind: bundle.ArtefactSecrets, Name: "wiki", Path: "secrets/wiki.json"}) {
		t.Fatalf("the secrets artefact = %+v", got)
	}
	if !reflect.DeepEqual(entry.Secrets.Names, []string{"encryption_key", "search/index_key", "session_key"}) {
		t.Errorf("status names %v", entry.Secrets.Names)
	}
	if m.SchemaVersion != 4 || m.Apps[0].Stores[1].Kind != bundle.ArtefactSecrets || m.Apps[0].Stores[1].Path != "secrets/wiki.json" {
		t.Errorf("the manifest: format %d, stores %+v", m.SchemaVersion, m.Apps[0].Stores)
	}

	cipher := w.bundles.objects[bundlestore.ImportBucket+"/20261007-101500-1a2b3c4d/secrets/wiki.json"]
	if !bytes.HasPrefix(cipher, []byte("age-encryption.org/v1")) {
		t.Fatalf("what was written is not an age file: %.40q", cipher)
	}
	manifest, _ := json.Marshal(m)
	status, _ := json.Marshal(w.export.Status)
	job, err := backup.ManifestJob(backup.JobParams{Name: "j", Namespace: "n", Bucket: "b", Prefix: "p", Encryption: passphraseEncryption}, m,
		backup.NewBundleInfo("demo", "nightly", "now", passphraseEncryption))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(job)
	for path, value := range values {
		for where, text := range map[string][]byte{"the artefact": cipher, "the manifest": manifest, "the export's status": status, "the manifest's Job": spec} {
			if bytes.Contains(text, []byte(value)) {
				t.Errorf("%s holds the value of %s in the clear", where, path)
			}
		}
	}

	// Whoever holds the bundle's key reads it, and nobody else.
	id, _ := age.NewScryptIdentity("correct horse")
	doc, err := backup.OpenAppSecrets(cipher, []age.Identity{id})
	if err != nil {
		t.Fatal(err)
	}
	want := &bundle.AppSecrets{App: "wiki",
		Secrets:    map[string]string{"encryption_key": values[wikiEncryption], "session_key": values[wikiSession]},
		Extensions: map[string]map[string]string{"search": {"index_key": values[wikiIndex]}}}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("the artefact holds names %v", heldNames(doc))
	}
	wrong, _ := age.NewScryptIdentity("another passphrase")
	if _, err := backup.OpenAppSecrets(cipher, []age.Identity{wrong}); err == nil {
		t.Error("the artefact opened with another passphrase")
	}

	// To the cluster's recipients as well, where that is how the bundle is
	// encrypted.
	identity, _ := age.GenerateX25519Identity()
	w.bundles.objects = nil
	recipients := backup.Encryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{identity.Recipient().String()}}
	again := gentianov1alpha1.AppExportStatus{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres}}}
	if err := w.er.withAppSecrets(context.Background(), w.export, planTenant("demo", "wiki"), "wiki", secretsProfile(), recipients, &again); err != nil {
		t.Fatal(err)
	}
	cipher = w.bundles.objects[bundlestore.ImportBucket+"/20261007-101500-1a2b3c4d/secrets/wiki.json"]
	if doc, err := backup.OpenAppSecrets(cipher, []age.Identity{identity}); err != nil || !reflect.DeepEqual(doc, want) {
		t.Errorf("encrypted to a recipient: %v", err)
	}
}

// What an export does not put into a bundle: the secrets of an app that has
// no data in it, a secret the vault does not hold, and anything at all when
// the vault cannot be read -- that is an error, not an app without secrets.
func TestAnExportCarriesNoSecretItCannotAccountFor(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	tenant := planTenant("demo", "wiki")

	noData := gentianov1alpha1.AppExportStatus{Name: "wiki"}
	if err := w.er.withAppSecrets(ctx, w.export, tenant, "wiki", secretsProfile(), passphraseEncryption, &noData); err != nil || len(noData.Artefacts) != 0 || len(w.bundles.objects) != 0 {
		t.Errorf("an app with no data in the bundle: %+v, %v", noData.Artefacts, err)
	}
	plain := gentianov1alpha1.AppExportStatus{Name: "drive", Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres}}}
	if err := w.er.withAppSecrets(ctx, w.export, tenant, "drive", chartProfile("drive", "1.0.0", "", false), passphraseEncryption, &plain); err != nil || len(plain.Artefacts) != 1 {
		t.Errorf("an app that declares no secret: %+v, %v", plain.Artefacts, err)
	}

	w.vault.purge("demo", "wiki-search")
	delete(w.vault.data, wikiSession)
	entry := gentianov1alpha1.AppExportStatus{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres}}}
	if err := w.er.withAppSecrets(ctx, w.export, tenant, "wiki", secretsProfile(), passphraseEncryption, &entry); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entry.Secrets.Names, []string{"encryption_key"}) {
		t.Errorf("with two of three never made, the bundle names %v", entry.Secrets.Names)
	}

	away := &TenantExportReconciler{Client: w.c, Bundles: w.bundles,
		Reconciler: &TenantReconciler{Client: w.c, Seeder: secrets.NewSeeder(memVaultAway{}, nil)}}
	failed := gentianov1alpha1.AppExportStatus{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres}}}
	if err := away.withAppSecrets(ctx, w.export, tenant, "wiki", secretsProfile(), passphraseEncryption, &failed); err == nil || len(failed.Artefacts) != 1 {
		t.Errorf("with the vault away: %+v, %v", failed.Artefacts, err)
	}
	// And with nothing to write through, an app that has secrets is not
	// captured as if it had none.
	w.er.Bundles = nil
	if err := w.er.withAppSecrets(ctx, w.export, tenant, "wiki", secretsProfile(), passphraseEncryption, &failed); err == nil {
		t.Error("an export with no way to write the secrets went on without them")
	}
}

// memVaultAway is a vault that does not answer.
type memVaultAway struct{}

func (memVaultAway) PutOnce(context.Context, string, map[string]string) error { return nil }
func (memVaultAway) Put(context.Context, string, map[string]string) error     { return nil }
func (memVaultAway) Get(context.Context, string) (map[string]string, error) {
	return nil, fmt.Errorf("vault away")
}

// The case the secrets travel for. On a cluster with random secrets an app
// is backed up, purged and installed again, which gives it new secrets; the
// bundle's data was written with the old ones. A restore sets them back,
// hands them to the app -- its Secret read again, the release that takes one
// as a value upgraded, the app restarted -- and what the app encrypted
// before the backup is readable again.
func TestRandomSecretsBackUpPurgeInstallRestoreAndTheDataIsReadable(t *testing.T) {
	w := newSecretsWorld(t, nil)
	before := w.vault.snapshot()
	data := appEncrypt(t, before[wikiEncryption], "what the wiki stored")

	w.bundles.manifest = w.backup(t, "demo")
	w.vault.purge("demo", "wiki")
	w.vault.purge("demo", "wiki-search")
	if len(w.vault.snapshot()) != 0 {
		t.Fatalf("the purge left %v", w.vault.snapshot())
	}
	w.install(t)
	fresh := w.vault.snapshot()
	for p, v := range fresh {
		if v == "" || v == before[p] {
			t.Fatalf("installed again, %s is %q; before the purge it was %q", p, v, before[p])
		}
	}
	if _, err := appDecrypt(fresh[wikiEncryption], data); err == nil {
		t.Fatal("the app reads its old data with a new secret: the test shows nothing")
	}

	got, upgraded := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if after := w.vault.snapshot(); !reflect.DeepEqual(after, before) {
		t.Errorf("the vault does not hold the values the data was written with")
	}
	if plain, err := appDecrypt(w.vault.value(wikiEncryption), data); err != nil || plain != "what the wiki stored" {
		t.Errorf("the app cannot read what it stored before the backup: %v", err)
	}
	// The app holds them: its Secret, and the release that takes one.
	secret := &corev1.Secret{}
	if err := w.c.Get(context.Background(), types.NamespacedName{Name: "wiki-sensitive-values", Namespace: "tenant-demo"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["internal-encryption_key"]) != before[wikiEncryption] || string(secret.Data["internal-session_key"]) != before[wikiSession] {
		t.Error("the app's Secret does not hold the restored values")
	}
	if !reflect.DeepEqual(upgraded, []string{"demo-wiki"}) {
		t.Errorf("releases upgraded = %v, want the one that is handed a replaced secret", upgraded)
	}
	if !w.restarted(t, "wiki") || w.restarted(t, "drive") {
		t.Errorf("restarted: wiki %v, drive %v", w.restarted(t, "wiki"), w.restarted(t, "drive"))
	}

	st := w.wikiStatus(t).Secrets
	if st == nil || st.DeliveredAt == nil || !reflect.DeepEqual(st.Replaced, []string{"encryption_key", "session_key", "search/index_key"}) ||
		!reflect.DeepEqual(st.Releases, []gentianov1alpha1.SecretsRelease{{Name: "demo-wiki", Revision: 3}}) {
		t.Errorf("status.secrets = %+v", st)
	}
	status, _ := json.Marshal(got.Status)
	for p, v := range before {
		if bytes.Contains(status, []byte(v)) {
			t.Errorf("the restore's status holds the value of %s", p)
		}
	}
	if notes := strings.Join(got.Status.Notes, "\n"); !strings.Contains(notes, "wiki: its own secrets encryption_key, session_key, search/index_key were replaced by the bundle's") {
		t.Errorf("the result does not say the secrets were replaced:\n%s", notes)
	}
	// Only the three paths were written, and each is the app's own.
	sort.Strings(w.vault.puts)
	if !reflect.DeepEqual(w.vault.puts, []string{wikiIndex, wikiEncryption, wikiSession}) {
		t.Errorf("paths written: %v", w.vault.puts)
	}
}

// An import: the bundle is of a tenant of another name, and the tenant it is
// restored into was made for it, with secrets of its own. The bundle's
// values go to the new tenant's paths for that app and to no other path:
// nothing is written under the old tenant's name, and nothing of another
// app's is touched.
func TestAnImportUnderANewNameSetsTheSecretsAtTheNewTenantsOwnPaths(t *testing.T) {
	w := newSecretsWorld(t, func(r *gentianov1alpha1.TenantRestore) { r.Spec.IntoNewTenant = true })
	ctx := context.Background()

	// The tenant the bundle was taken of, on its own cluster.
	source := newSecretVault()
	old := secrets.NewSeeder(source, nil).WithMode(randomMode)
	oldTenant := planTenant("old", "wiki")
	if err := (&TenantReconciler{Seeder: old}).seedAppSecrets(ctx, oldTenant, "wiki", secretsProfile()); err != nil {
		t.Fatal(err)
	}
	was := source.value("gentian-os/tenants/old/apps/wiki/internal/encryption_key")
	data := appEncrypt(t, was, "what the wiki stored")
	exporter := &TenantExportReconciler{Client: w.c, Reconciler: &TenantReconciler{Client: w.c, Seeder: old}, Bundles: w.bundles}
	entry := gentianov1alpha1.AppExportStatus{Name: "wiki", ChartVersion: "2.0.0",
		Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres, Name: "old_wiki", Path: backup.PostgresArtefact("old_wiki")}}}
	if err := exporter.withAppSecrets(ctx, w.export, oldTenant, "wiki", secretsProfile(), passphraseEncryption, &entry); err != nil {
		t.Fatal(err)
	}
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{entry}
	m := exporter.buildManifest(w.export, oldTenant)
	m.Identity, m.Shell = nil, nil
	w.bundles.manifest = m

	// Another app of the new tenant, and another tenant, hold secrets too.
	_, _ = w.seeder.SeedAppSecret(ctx, "demo", "drive", "encryption_key")
	_, _ = w.seeder.SeedAppSecret(ctx, "other", "wiki", "encryption_key")
	untouched := map[string]string{}
	for _, p := range []string{"gentian-os/tenants/demo/apps/drive/internal/encryption_key", "gentian-os/tenants/other/apps/wiki/internal/encryption_key"} {
		untouched[p] = w.vault.value(p)
	}

	got, _ := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if plain, err := appDecrypt(w.vault.value(wikiEncryption), data); err != nil || plain != "what the wiki stored" {
		t.Errorf("the imported app cannot read the data the bundle brought: %v", err)
	}
	for p, v := range untouched {
		if w.vault.value(p) != v {
			t.Errorf("%s was changed", p)
		}
	}
	for p := range w.vault.snapshot() {
		if strings.HasPrefix(p, "gentian-os/tenants/old/") {
			t.Errorf("%s was written: the bundle's tenant has no place in this vault", p)
		}
	}
	sort.Strings(w.vault.puts)
	if !reflect.DeepEqual(w.vault.puts, []string{wikiIndex, wikiEncryption, wikiSession}) {
		t.Errorf("paths written: %v", w.vault.puts)
	}
}

// A bundle says names and values. Whatever names it holds, a restore writes
// under the names the app's profile declares here and under no other: not a
// name the profile does not have, not an extension it does not have, and not
// a name made to look like a path. Each is named in the result as not
// written.
func TestARestoreWritesOnlyUnderNamesTheProfileDeclares(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	before := w.vault.snapshot()

	doc := &bundle.AppSecrets{App: "wiki",
		Secrets: map[string]string{
			"encryption_key":                         "from-the-bundle",
			"admin_password":                         "not-declared",
			"../../drive/internal/encryption_key":    "a-path",
			"../../../../kernel/backup/identity":     "a-kernel-path",
			"gentian-os/tenants/other/apps/wiki/x/y": "an-absolute-path",
		},
		Extensions: map[string]map[string]string{
			"search":  {"index_key": "from-the-bundle-too", "other": "not-declared"},
			"../kern": {"index_key": "not-an-extension"},
		}}
	cipher, err := backup.SealAppSecrets(passphraseEncryption, "correct horse", doc)
	if err != nil {
		t.Fatal(err)
	}
	ref := *w.restore(t).Spec.Bundle
	_ = w.bundles.PutArtefact(ctx, ref, backup.SecretsArtefact("wiki"), cipher)
	m := wikiManifest()
	m.SchemaVersion, m.Tenant = 4, "demo"
	m.Apps = m.Apps[:1]
	m.Apps[0].Stores = []backup.ManifestStore{
		{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"},
		{Kind: bundle.ArtefactSecrets, Name: "wiki", Path: "secrets/wiki.json"},
	}
	m.Identity, m.Shell = nil, nil
	w.bundles.manifest = m

	got, _ := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	after := w.vault.snapshot()
	want := map[string]string{wikiEncryption: "from-the-bundle", wikiSession: before[wikiSession], wikiIndex: "from-the-bundle-too"}
	if !reflect.DeepEqual(after, want) {
		t.Errorf("the vault holds paths %v", pathsOf(after))
	}
	sort.Strings(w.vault.puts)
	if !reflect.DeepEqual(w.vault.puts, []string{wikiIndex, wikiEncryption}) {
		t.Errorf("paths written: %v", w.vault.puts)
	}
	st := w.wikiStatus(t).Secrets
	if !reflect.DeepEqual(st.Names, []string{"encryption_key", "search/index_key"}) || len(st.NotDeclared) != 6 {
		t.Errorf("status.secrets: set %v, not declared %v", st.Names, st.NotDeclared)
	}
	if notes := strings.Join(got.Status.Notes, "\n"); !strings.Contains(notes, "which the app's profile here does not declare: not written") ||
		!strings.Contains(notes, "admin_password") {
		t.Errorf("the result does not name what was not written:\n%s", notes)
	}
}

func pathsOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Where the vault holds the bundle's values already -- a restore into the
// tenant the bundle was taken of, nothing purged -- nothing is written,
// nothing is told to read again, no release is touched, and the app is not
// restarted on the secrets' account.
func TestARestoreThatFindsTheSameSecretsChangesNothing(t *testing.T) {
	w := newSecretsWorld(t, nil)
	w.bundles.manifest = w.backup(t, "demo")

	got, upgraded := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	st := w.wikiStatus(t).Secrets
	if len(w.vault.puts) != 0 || len(upgraded) != 0 || st == nil || len(st.Replaced) != 0 || len(st.Releases) != 0 || st.DeliveredAt == nil {
		t.Errorf("written %v, upgraded %v, status %+v", w.vault.puts, upgraded, st)
	}
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	if err := w.c.Get(context.Background(), types.NamespacedName{Name: "wiki-sensitive-values", Namespace: "tenant-demo"}, es); err != nil {
		t.Fatal(err)
	}
	if es.GetAnnotations()[forceSyncAnnotation] != "" {
		t.Error("the app's ExternalSecret was told to read again")
	}
	if notes := strings.Join(got.Status.Notes, "\n"); !strings.Contains(notes, "wiki: its own secrets are the bundle's already; none was changed") {
		t.Errorf("the notes:\n%s", notes)
	}
}

// A restore that set the secrets and ended before the app was handed them
// leaves the vault with the bundle's values and the app's Secret with the
// old ones. The next restore finds nothing to replace in the vault, and
// still hands the app the values before it replaces its data.
func TestARestoreHandsTheAppASecretTheVaultAlreadyHolds(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	w.bundles.manifest = w.backup(t, "demo")
	secret := &corev1.Secret{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "wiki-sensitive-values", Namespace: "tenant-demo"}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["internal-encryption_key"] = []byte("what-the-app-still-runs-with")
	if err := w.c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}

	got, upgraded := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if st := w.wikiStatus(t).Secrets; len(w.vault.puts) != 0 || !reflect.DeepEqual(st.Replaced, []string{"encryption_key"}) ||
		!reflect.DeepEqual(upgraded, []string{"demo-wiki"}) || !w.restarted(t, "wiki") {
		t.Errorf("written %v, handed anew %v, upgraded %v, restarted %v", w.vault.puts, st.Replaced, upgraded, w.restarted(t, "wiki"))
	}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "wiki-sensitive-values", Namespace: "tenant-demo"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["internal-encryption_key"]) != w.vault.value(wikiEncryption) {
		t.Error("the app's Secret does not hold the vault's value")
	}
}

// A bundle of an older format holds no secrets, and still restores, as it
// did: no stored secret is changed, and the result says the data an app
// encrypted is readable only where its secrets are the ones it was written
// with.
func TestABundleWithoutSecretsRestoresAndChangesNoSecret(t *testing.T) {
	for _, version := range []int{2, 3} {
		w := newSecretsWorld(t, nil)
		before := w.vault.snapshot()
		m := wikiManifest()
		m.SchemaVersion = version
		w.bundles.manifest = m

		got, upgraded := w.runWithOperators(t)
		if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
			t.Fatalf("format %d: phase = %s: %+v", version, got.Status.Phase, got.Status.Conditions)
		}
		if !reflect.DeepEqual(w.vault.snapshot(), before) || len(w.vault.puts) != 0 || len(upgraded) != 0 || w.wikiStatus(t).Secrets != nil {
			t.Errorf("format %d: the restore touched the app's secrets: written %v, upgraded %v, status %+v", version, w.vault.puts, upgraded, w.wikiStatus(t).Secrets)
		}
		notes := strings.Join(got.Status.Notes, "\n")
		for _, must := range []string{
			fmt.Sprintf("Stored credentials are not in this bundle: it is of format %d", version),
			"wiki: the bundle holds none of its own secrets, which are left as they are",
		} {
			if !strings.Contains(notes, must) {
				t.Errorf("format %d: the notes do not say %q:\n%s", version, must, notes)
			}
		}
	}
}

// The steps a replaced secret takes to the app, one pass at a time: the
// vault first, with the record of what will change written before it; then
// the app's Secret, which the restore waits for; only then the release, told
// to look once; and the app is not paused before all of it is done. A secret
// that does not arrive fails the restore of the app with its data untouched.
func TestAReplacedSecretReachesTheAppBeforeItsDataIsReplaced(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	before := w.vault.snapshot()
	w.bundles.manifest = w.backup(t, "demo")
	w.vault.purge("demo", "wiki")
	w.install(t)

	pass := func() {
		t.Helper()
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
	}
	release := func() *unstructured.Unstructured {
		t.Helper()
		r := &unstructured.Unstructured{}
		r.SetGroupVersionKind(helmReleaseGVK)
		if err := w.c.Get(ctx, types.NamespacedName{Name: "demo-wiki"}, r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	pass() // the plan
	pass() // the vault
	st := w.wikiStatus(t)
	if w.vault.value(wikiEncryption) != before[wikiEncryption] || st.Secrets.AppliedAt == nil || st.Secrets.DeliveredAt != nil || st.QuiesceStart != nil {
		t.Fatalf("after the first passes: applied %v, delivered %v, paused %v", st.Secrets.AppliedAt, st.Secrets.DeliveredAt, st.QuiesceStart)
	}
	if !reflect.DeepEqual(st.Secrets.Replaced, []string{"encryption_key", "session_key"}) {
		t.Errorf("replaced = %v; the extension's was not purged and is the bundle's", st.Secrets.Replaced)
	}
	pass()
	if st := w.wikiStatus(t); !strings.Contains(st.Message, "waiting on Secret wiki-sensitive-values") || st.QuiesceStart != nil {
		t.Errorf("with the Secret not yet filled: %q, paused %v", st.Message, st.QuiesceStart)
	}
	if release().GetAnnotations()[secretsRestoredAnnotation] != "" {
		t.Error("the release was told to look before the Secret it reads held the new value")
	}
	if len(w.jobs(t)) != 0 {
		t.Errorf("a restore Job was made before the app held its secrets: %d", len(w.jobs(t)))
	}

	w.playSecretsOperator(t, false)
	pass()
	mark := release().GetAnnotations()[secretsRestoredAnnotation]
	if st := w.wikiStatus(t); mark == "" || !strings.Contains(st.Message, "waiting on release demo-wiki") || st.QuiesceStart != nil {
		t.Errorf("with the Secret filled: mark %q, %q, paused %v", mark, st.Message, st.QuiesceStart)
	}
	pass()
	if release().GetAnnotations()[secretsRestoredAnnotation] != mark || release().GetResourceVersion() != func() string { pass(); return release().GetResourceVersion() }() {
		t.Error("the release is told to look again on every pass")
	}

	w.playHelmProvider(t)
	pass()
	if st := w.wikiStatus(t); st.Secrets.DeliveredAt == nil || st.QuiesceStart == nil {
		t.Errorf("with the release upgraded: delivered %v, paused %v", st.Secrets.DeliveredAt, st.QuiesceStart)
	}

	// And one that never arrives.
	late := newSecretsWorld(t, nil)
	late.bundles.manifest = late.backup(t, "demo")
	late.vault.purge("demo", "wiki")
	late.install(t)
	for i := 0; i < 2; i++ {
		if _, err := late.r.Reconcile(ctx, ctrl.Request{NamespacedName: late.key}); err != nil {
			t.Fatal(err)
		}
	}
	stuck := late.restore(t)
	for i := range stuck.Status.Apps {
		if stuck.Status.Apps[i].Name == "wiki" {
			long := metav1.NewTime(time.Now().Add(-restoreSecretsWait - time.Minute))
			stuck.Status.Apps[i].Secrets.AppliedAt = &long
		}
	}
	if err := late.c.Status().Update(ctx, stuck); err != nil {
		t.Fatal(err)
	}
	if _, err := late.r.Reconcile(ctx, ctrl.Request{NamespacedName: late.key}); err != nil {
		t.Fatal(err)
	}
	failed := late.restore(t)
	if failed.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed || len(late.jobs(t)) != 0 {
		t.Fatalf("phase = %s with %d Job(s)", failed.Status.Phase, len(late.jobs(t)))
	}
	if message := conditionMessage(failed.Status.Conditions, conditionExportComplete); !strings.Contains(message, "did not reach the app") || !strings.Contains(message, "data was not touched") {
		t.Errorf("the failure says: %s", message)
	}
}

func conditionMessage(conditions []metav1.Condition, kind string) string {
	for _, c := range conditions {
		if c.Type == kind {
			return c.Message
		}
	}
	return ""
}

// A manifest comes from a bundle, and a bundle from anywhere. One that files
// secrets under an app at a place that is not that app's own is not gone by:
// the restore is refused before anything is changed.
func TestAManifestThatMisfilesSecretsIsRefused(t *testing.T) {
	tenant := planTenant("demo", "wiki")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"wiki": secretsProfile()}
	for name, store := range map[string]backup.ManifestStore{
		"another app's file":  {Kind: bundle.ArtefactSecrets, Name: "wiki", Path: "secrets/drive.json"},
		"another app's name":  {Kind: bundle.ArtefactSecrets, Name: "drive", Path: "secrets/wiki.json"},
		"a path of its own":   {Kind: bundle.ArtefactSecrets, Name: "wiki", Path: "postgres/demo_wiki.pgc"},
		"a path out of there": {Kind: bundle.ArtefactSecrets, Name: "wiki", Path: "../secrets/wiki.json"},
	} {
		m := &backup.Manifest{SchemaVersion: 4, Tenant: "demo", Apps: []backup.ManifestApp{{Name: "wiki", ChartVersion: "2.0.0",
			Stores: []backup.ManifestStore{{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"}, store}}}}
		if _, err := planRestore(m, tenant, nil, false, targetOf(tenant), liveFrom(tenant, profiles, nil)); err == nil {
			t.Errorf("%s: planned", name)
		}
	}
	// As an export writes it, it is planned: the artefact with no target,
	// and the plan says what will be done.
	m := &backup.Manifest{SchemaVersion: 4, Tenant: "demo", Apps: []backup.ManifestApp{{Name: "wiki", ChartVersion: "2.0.0",
		Stores: []backup.ManifestStore{{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: "postgres/demo_wiki.pgc"},
			{Kind: bundle.ArtefactSecrets, Name: "wiki", Path: backup.SecretsArtefact("wiki")}}}}}
	plan, err := planRestore(m, tenant, nil, false, targetOf(tenant), liveFrom(tenant, profiles, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := plan.apps[0].artefacts[1]
	if got != (gentianov1alpha1.BundleArtefact{Kind: bundle.ArtefactSecrets, Name: "wiki", Path: "secrets/wiki.json"}) {
		t.Errorf("planned %+v", got)
	}
	if !strings.Contains(plan.apps[0].note, "each one its profile declares here is set to the bundle's value") {
		t.Errorf("the plan's note: %s", plan.apps[0].note)
	}
}

// The data of an app that was uninstalled with its data kept comes with the
// secrets it was written with: they are set at the app's place in the vault,
// where its next install finds them and keeps them. Nothing runs, so nothing
// is waited for.
func TestAnUninstalledAppsSecretsAreSetForItsNextInstall(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	held := w.vault.snapshot()
	doc := &bundle.AppSecrets{App: "wiki", Secrets: map[string]string{"encryption_key": "written-with", "session_key": held[wikiSession]}}
	cipher, err := backup.SealAppSecrets(passphraseEncryption, "correct horse", doc)
	if err != nil {
		t.Fatal(err)
	}
	restore := w.restore(t)
	restore.Status.Bundle = restore.Spec.Bundle
	_ = w.bundles.PutArtefact(ctx, *restore.Status.Bundle, backup.SecretsArtefact("wiki"), cipher)
	restore.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: "wiki", Retained: true,
		Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactSecrets, Name: "wiki", Path: "secrets/wiki.json"}}}}

	ready, failure, err := w.r.restoreAppSecrets(ctx, restore, planTenant("demo"), "wiki", secretsProfile())
	entry := &restore.Status.Apps[0]
	if !ready || failure != "" || err != nil {
		t.Fatalf("ready = %v, %q, %v", ready, failure, err)
	}
	if w.vault.value(wikiEncryption) != "written-with" || !reflect.DeepEqual(entry.Secrets.Replaced, []string{"encryption_key"}) ||
		len(entry.Secrets.Releases) != 0 || entry.Secrets.DeliveredAt == nil {
		t.Errorf("status %+v", entry.Secrets)
	}
	// The install that follows keeps it.
	if got, err := w.seeder.SeedAppSecret(ctx, "demo", "wiki", "encryption_key"); err != nil || got != "written-with" {
		t.Errorf("the next install holds %q, %v", got, err)
	}
	if note := secretsNote(entry); !strings.Contains(note, "for its next install") {
		t.Errorf("the note: %s", note)
	}
}

// Without a vault there is nowhere to set them, and a restore says so rather
// than putting back data its app cannot read.
func TestARestoreOfSecretsWithoutAVaultFails(t *testing.T) {
	w := newSecretsWorld(t, nil)
	w.bundles.manifest = w.backup(t, "demo")
	w.r.Tenant.Seeder = nil
	got, _ := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed ||
		!strings.Contains(conditionMessage(got.Status.Conditions, conditionExportComplete), "no vault to set them in") || len(w.jobs(t)) != 0 {
		t.Errorf("phase %s: %s", got.Status.Phase, conditionMessage(got.Status.Conditions, conditionExportComplete))
	}
}

// A key a profile declares under spec.secrets.derived is kept in the vault
// like a generated secret, an app may encrypt with it, and it travels the
// same way: in the app's secrets artefact, apart from the generated ones,
// and set again by a restore at the app's own derived/ path in the tenant
// restored into, under a key the profile declares there and no other. It
// reaches the app in the Secret the operator writes itself
// (llm-credentials-<app>), which the restore writes and waits for, with the
// release that takes the key as a value.
func TestADeclaredKeyTravelsAndIsSetLikeAGeneratedSecret(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	const keyPath = "gentian-os/tenants/demo/apps/wiki/derived/WIKI_SECRET_KEY"
	tenant := planTenant("demo", "wiki", "drive")
	profile := secretsProfile()
	profile.Spec.Secrets.Derived = []gentianov1alpha1.DerivedSecretKey{{Key: "WIKI_SECRET_KEY"}}
	stored := &gentianov1alpha1.ComponentProfile{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "wiki"}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Secrets = profile.Spec.Secrets
	if err := w.c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	// As the tenant's reconcile delivers it: in the operator's own Secret.
	deliver := func() string {
		t.Helper()
		value, err := w.seeder.SeedDerivedKey(ctx, "demo", "wiki", "WIKI_SECRET_KEY")
		if err != nil {
			t.Fatal(err)
		}
		if err := w.r.Tenant.writeModelCredentials(ctx, tenant, "wiki", map[string]string{"WIKI_SECRET_KEY": value, "OPENAI_API_KEY": "sk-unrelated"}); err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := deliver()
	release := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"forProvider": map[string]any{"set": []any{
			map[string]any{"name": "secretKey", "valueFrom": map[string]any{"secretKeyRef": map[string]any{
				"name": "llm-credentials-wiki", "namespace": "tenant-demo", "key": "WIKI_SECRET_KEY"}}},
		}}},
		"status": map[string]any{"atProvider": map[string]any{"revision": int64(3)},
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}
	release.SetGroupVersionKind(helmReleaseGVK)
	release.SetName("demo-wiki-llm")
	if err := w.c.Create(ctx, release); err != nil {
		t.Fatal(err)
	}
	data := appEncrypt(t, first, "a token the wiki keeps for a person")

	// Backed up: the key is in the artefact, in a section of its own.
	entry := gentianov1alpha1.AppExportStatus{Name: "wiki", ChartVersion: "2.0.0",
		Artefacts: []gentianov1alpha1.BundleArtefact{{Kind: bundle.ArtefactPostgres, Name: "demo_wiki", Path: backup.PostgresArtefact("demo_wiki")}}}
	if err := w.er.withAppSecrets(ctx, w.export, tenant, "wiki", profile, passphraseEncryption, &entry); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entry.Secrets.Names, []string{"derived:WIKI_SECRET_KEY", "encryption_key", "search/index_key", "session_key"}) {
		t.Fatalf("captured %v", entry.Secrets.Names)
	}
	ref := *w.restore(t).Spec.Bundle
	doc, err := w.bundles.AppSecrets(ctx, ref, "secrets/wiki.json", bundlestore.Key{Passphrase: "correct horse"})
	if err != nil || !reflect.DeepEqual(doc.Derived, map[string]string{"WIKI_SECRET_KEY": first}) || doc.Secrets["WIKI_SECRET_KEY"] != "" {
		t.Fatalf("the artefact's declared keys: %v, %v", err, len(doc.Derived))
	}
	// And one the profile here does not declare, with a name made to be a path.
	doc.Derived["OTHER_KEY"] = "not-declared"
	doc.Derived["../internal/encryption_key"] = "a-path"
	cipher, err := backup.SealAppSecrets(passphraseEncryption, "correct horse", doc)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.bundles.PutArtefact(ctx, ref, "secrets/wiki.json", cipher)
	w.export.Status.Apps = []gentianov1alpha1.AppExportStatus{entry}
	m := w.er.buildManifest(w.export, planTenant("demo", "wiki"))
	m.Identity, m.Shell = nil, nil
	w.bundles.manifest = m
	status, _ := json.Marshal(w.export.Status)
	manifest, _ := json.Marshal(m)
	if bytes.Contains(status, []byte(first)) || bytes.Contains(manifest, []byte(first)) || bytes.Contains(w.bundles.objects[ref.Bucket+"/"+ref.Prefix+"/secrets/wiki.json"], []byte(first)) {
		t.Error("the key's value is in the clear in the status, the manifest or the artefact")
	}

	// Purged and installed again: another key, and the old data unreadable.
	generated := w.vault.snapshot()
	w.vault.purge("demo", "wiki")
	if err := w.r.Tenant.seedAppSecrets(ctx, tenant, "wiki", profile); err != nil {
		t.Fatal(err)
	}
	w.playSecretsOperator(t, true)
	second := deliver()
	if second == first {
		t.Fatal("installed again, the key is the one it was")
	}
	if _, err := appDecrypt(second, data); err == nil {
		t.Fatal("the app reads its old data with a new key")
	}
	w.vault.puts = nil

	got, upgraded := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if plain, err := appDecrypt(w.vault.value(keyPath), data); err != nil || plain != "a token the wiki keeps for a person" {
		t.Errorf("restored, the app cannot read what it encrypted with the key: %v", err)
	}
	secret := &corev1.Secret{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "llm-credentials-wiki", Namespace: "tenant-demo"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["WIKI_SECRET_KEY"]) != first || string(secret.Data["OPENAI_API_KEY"]) != "sk-unrelated" {
		t.Error("the app's Secret does not hold the restored key, or lost what else it held")
	}
	sort.Strings(upgraded)
	if !reflect.DeepEqual(upgraded, []string{"demo-wiki", "demo-wiki-llm"}) || !w.restarted(t, "wiki") {
		t.Errorf("upgraded %v, restarted %v", upgraded, w.restarted(t, "wiki"))
	}
	st := w.wikiStatus(t).Secrets
	if !slices.Contains(st.Replaced, "derived:WIKI_SECRET_KEY") || !reflect.DeepEqual(st.NotDeclared, []string{"derived:../internal/encryption_key", "derived:OTHER_KEY"}) {
		t.Errorf("status: replaced %v, not declared %v", st.Replaced, st.NotDeclared)
	}
	// The key's own path and the generated secrets' were written, nothing
	// else; the generated ones hold what they held before the purge.
	sort.Strings(w.vault.puts)
	if !reflect.DeepEqual(w.vault.puts, []string{keyPath, wikiEncryption, wikiSession}) {
		t.Errorf("paths written: %v", w.vault.puts)
	}
	if after := w.vault.snapshot(); !reflect.DeepEqual(after, generated) {
		t.Errorf("the generated secrets are not the ones before the purge")
	}
	for p := range w.vault.data {
		if strings.Contains(p, "OTHER_KEY") || strings.Contains(p, "..") {
			t.Errorf("%s was written", p)
		}
	}
	final, _ := json.Marshal(got.Status)
	if bytes.Contains(final, []byte(first)) {
		t.Error("the restore's status holds the key")
	}
}

// A bundle whose secrets artefact was written before declared keys
// travelled holds none. It restores as before: the key stays as it is, and
// the result says so.
func TestABundleWithoutDeclaredKeysLeavesThemAndSaysSo(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	w.bundles.manifest = w.backup(t, "demo") // the profile declared no key then
	stored := &gentianov1alpha1.ComponentProfile{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "wiki"}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Secrets.Derived = []gentianov1alpha1.DerivedSecretKey{{Key: "WIKI_SECRET_KEY"}}
	if err := w.c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	held, err := w.seeder.SeedDerivedKey(ctx, "demo", "wiki", "WIKI_SECRET_KEY")
	if err != nil {
		t.Fatal(err)
	}

	got, _ := w.runWithOperators(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if w.vault.value("gentian-os/tenants/demo/apps/wiki/derived/WIKI_SECRET_KEY") != held || len(w.vault.puts) != 0 {
		t.Errorf("the key was changed: written %v", w.vault.puts)
	}
	if st := w.wikiStatus(t).Secrets; !reflect.DeepEqual(st.NotHeld, []string{"derived:WIKI_SECRET_KEY"}) {
		t.Errorf("status.secrets.notHeld = %v", st.NotHeld)
	}
	if notes := strings.Join(got.Status.Notes, "\n"); !strings.Contains(notes, "the bundle holds no value for derived:WIKI_SECRET_KEY, which the app's profile here declares: left as it is") {
		t.Errorf("the notes:\n%s", notes)
	}
}
