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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func exportReconciler(t *testing.T, objs ...client.Object) *TenantExportReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("gentian scheme: %v", err)
	}
	return &TenantExportReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Scheme: s,
	}
}

func exportWith(enc *gentianov1alpha1.ExportEncryption) *gentianov1alpha1.TenantExport {
	return &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
		Spec:       gentianov1alpha1.TenantExportSpec{Encryption: enc},
	}
}

func passphraseSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-passphrase", Namespace: "tenant-demo"},
		Data:       map[string][]byte{"passphrase": []byte("correct horse battery staple")},
	}
}

// With no recipients configured anywhere, an export must fail rather than fall
// back to writing a tenant's data in the clear.
func TestExportRefusesWhenNoRecipientIsConfigured(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "")
	r := exportReconciler(t)

	if _, err := r.resolveEncryption(context.Background(), exportWith(nil)); err == nil {
		t.Fatal("resolveEncryption accepted an export it cannot protect")
	}
}

// The default mode needs no input at all, which is what lets a scheduled export
// run with nobody present.
func TestDefaultModeUsesClusterRecipientsUnattended(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "age1clusterkey, age1secondkey ")
	r := exportReconciler(t)
	export := exportWith(nil)

	enc, err := r.resolveEncryption(context.Background(), export)
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	if enc.Mode != gentianov1alpha1.ExportEncryptionRecipient {
		t.Errorf("mode = %q, want recipient", enc.Mode)
	}
	if len(enc.Recipients) != 2 || enc.Recipients[0] != "age1clusterkey" {
		t.Errorf("recipients = %v (whitespace should be trimmed)", enc.Recipients)
	}

	recordEncryption(export, enc, []string{"age1clusterkey", "age1secondkey"})
	if !export.Status.Encryption.PlatformReadable {
		t.Error("a bundle encrypted to the cluster key must report as platform-readable")
	}
}

// An admin naming their own recipient takes the platform's access away, and the
// status has to say so — support reading it will otherwise promise a recovery
// they cannot perform.
func TestOwnRecipientReplacesClusterAndDropsPlatformAccess(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "age1clusterkey")
	r := exportReconciler(t)
	export := exportWith(&gentianov1alpha1.ExportEncryption{
		Mode:       gentianov1alpha1.ExportEncryptionRecipient,
		Recipients: []string{"age1adminkey"},
	})

	enc, err := r.resolveEncryption(context.Background(), export)
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	if len(enc.Recipients) != 1 || enc.Recipients[0] != "age1adminkey" {
		t.Errorf("recipients = %v, want only the admin's key", enc.Recipients)
	}

	recordEncryption(export, enc, []string{"age1clusterkey"})
	if export.Status.Encryption.PlatformReadable {
		t.Error("platform must not claim it can read a bundle it has no identity for")
	}
}

func TestPassphraseIsStagedBesideTheJobsAndThenDiscarded(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "")
	r := exportReconciler(t, passphraseSecret())
	export := exportWith(&gentianov1alpha1.ExportEncryption{
		Mode:                gentianov1alpha1.ExportEncryptionPassphrase,
		PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "my-passphrase"},
	})
	ctx := context.Background()

	enc, err := r.resolveEncryption(ctx, export)
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	if enc.PassphraseSecret != "tx-nightly-passphrase" {
		t.Errorf("staged secret = %q", enc.PassphraseSecret)
	}

	// Jobs run in the kernel namespace and cannot read across namespaces, so a
	// copy has to exist there — with the same value.
	staged := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: "tx-nightly-passphrase", Namespace: kernelNamespace,
	}, staged); err != nil {
		t.Fatalf("passphrase was not staged: %v", err)
	}
	if string(staged.Data["passphrase"]) != "correct horse battery staple" {
		t.Errorf("staged passphrase = %q", staged.Data["passphrase"])
	}

	// And it must not outlive the export: retaining it would quietly undo the
	// only guarantee this mode exists to provide.
	if err := r.discardPassphrase(ctx, export); err != nil {
		t.Fatalf("discardPassphrase: %v", err)
	}
	err = r.Get(ctx, types.NamespacedName{
		Name: "tx-nightly-passphrase", Namespace: kernelNamespace,
	}, staged)
	if !apierrors.IsNotFound(err) {
		t.Errorf("passphrase survived the export: %v", err)
	}

	// Discarding twice is normal — it runs on several terminal paths.
	if err := r.discardPassphrase(ctx, export); err != nil {
		t.Errorf("second discard failed: %v", err)
	}
}

func TestPassphraseModeRejectsMissingOrEmptyInput(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "age1clusterkey")
	ctx := context.Background()

	// No reference at all.
	r := exportReconciler(t)
	if _, err := r.resolveEncryption(ctx, exportWith(&gentianov1alpha1.ExportEncryption{
		Mode: gentianov1alpha1.ExportEncryptionPassphrase,
	})); err == nil {
		t.Error("accepted passphrase mode with no secret reference")
	}

	// Reference to a Secret that does not exist.
	if _, err := r.resolveEncryption(ctx, exportWith(&gentianov1alpha1.ExportEncryption{
		Mode:                gentianov1alpha1.ExportEncryptionPassphrase,
		PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "absent"},
	})); err == nil {
		t.Error("accepted a reference to a missing Secret")
	}

	// Present but empty: an empty passphrase would encrypt to something anyone
	// can open, which is worse than failing.
	empty := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "blank", Namespace: "tenant-demo"},
		Data:       map[string][]byte{"passphrase": []byte("")},
	}
	r2 := exportReconciler(t, empty)
	if _, err := r2.resolveEncryption(ctx, exportWith(&gentianov1alpha1.ExportEncryption{
		Mode:                gentianov1alpha1.ExportEncryptionPassphrase,
		PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "blank"},
	})); err == nil {
		t.Error("accepted an empty passphrase")
	}
}

func TestPassphraseHonoursACustomKey(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "tenant-demo"},
		Data:       map[string][]byte{"pw": []byte("s3cret")},
	}
	r := exportReconciler(t, secret)

	enc, err := r.resolveEncryption(context.Background(), exportWith(&gentianov1alpha1.ExportEncryption{
		Mode: gentianov1alpha1.ExportEncryptionPassphrase,
		PassphraseSecretRef: &gentianov1alpha1.SecretKeyRef{
			Name: "custom",
			Key:  "pw",
		},
	}))
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	if enc.PassphraseKey != "pw" {
		t.Errorf("passphrase key = %q, want pw", enc.PassphraseKey)
	}
}

// The default mode is the platform key, and it is the only mode a schedule can
// use — a passphrase has nobody to type it at 03:00. Nothing generated one, and
// the value that carries it is per-cluster Helm config, so a cluster nobody had
// hand-edited had no key: its nightly backup failed every night, instantly,
// telling an administrator to go and configure something.
//
// The recovery kit now writes the public half here when it makes the identity,
// which is the step whose output already has to leave the cluster.
func TestRecipientsComeFromOpenBaoBeforeTheEnvironment(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "age1fromenv")

	// No OpenBao wired up: the environment is still honoured, so a cluster that
	// sets the value explicitly keeps working.
	r := &TenantExportReconciler{}
	got := r.clusterRecipients(context.Background())
	if len(got) != 1 || got[0] != "age1fromenv" {
		t.Fatalf("recipients = %v, want the environment's when OpenBao has none", got)
	}
}

func TestRecipientsAreParsedAndTrimmed(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 0},
		{"age1one", 1},
		{" age1one , age1two ", 2},
		{"age1one,,age1two", 2},
	} {
		if got := splitRecipients(tc.raw); len(got) != tc.want {
			t.Errorf("splitRecipients(%q) = %v, want %d", tc.raw, got, tc.want)
		}
	}
	if got := splitRecipients(" age1one , age1two "); got[0] != "age1one" || got[1] != "age1two" {
		t.Errorf("whitespace was not trimmed: %v", got)
	}
}
