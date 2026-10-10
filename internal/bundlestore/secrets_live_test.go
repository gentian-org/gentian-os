/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bundlestore

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/minio/minio-go/v7"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// An app's own secrets against an object store and a vault.
//
// What the operator does with them -- reads them from the vault, encrypts
// them, writes the artefact, reads it back, opens it, replaces the stored
// value -- against a MinIO server and an OpenBao server, with secrets drawn
// at random as under secretMode: random. It needs docker, for MinIO, and the
// bao binary, and is run when asked for:
//
//	GENTIAN_LIVE_BUNDLE_SECRETS=1 go test ./internal/bundlestore -run TestAppSecretsAgainstAnObjectStoreAndAVault -v
//
// GENTIAN_LIVE_MINIO_IMAGE names another MinIO image than the chart's.

const (
	liveMinioUser = "live-access"
	liveMinioPass = "live-secret-key"
	liveBaoToken  = "live-root"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, what, url string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		resp, err := http.Get(url) //nolint:gosec,noctx // a local test server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not come up at %s: %v", what, url, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// liveMinioImage is the image of the chart the platform deploys, unless the
// environment names another.
func liveMinioImage(t *testing.T) string {
	t.Helper()
	if image := os.Getenv("GENTIAN_LIVE_MINIO_IMAGE"); image != "" {
		return image
	}
	values, err := os.ReadFile(filepath.Join("..", "..", "charts", "infra", "minio", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	image := regexp.MustCompile(`(?m)^  repository: (\S+/minio)\n  tag: (\S+)`).FindSubmatch(values)
	if image == nil {
		t.Fatal("the minio chart's values pin no image")
	}
	return string(image[1]) + ":" + string(image[2])
}

func startLiveMinio(t *testing.T) string {
	t.Helper()
	port := freePort(t)
	name := fmt.Sprintf("gentian-minio-live-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-e", "MINIO_ROOT_USER="+liveMinioUser, "-e", "MINIO_ROOT_PASSWORD="+liveMinioPass,
		liveMinioImage(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitFor(t, "MinIO", endpoint+"/minio/health/live")
	return endpoint
}

func startLiveBao(t *testing.T) *secrets.KVClient {
	t.Helper()
	if _, err := exec.LookPath("bao"); err != nil {
		t.Skip("the bao binary is not on the PATH")
	}
	port := freePort(t)
	cmd := exec.Command("bao", "server", "-dev", "-dev-root-token-id="+liveBaoToken,
		fmt.Sprintf("-dev-listen-address=127.0.0.1:%d", port))
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatalf("bao server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitFor(t, "OpenBao", addr+"/v1/sys/health")
	kv := secrets.NewKVClient(addr, "unused", "")
	kv.SetStaticToken(liveBaoToken)
	return kv
}

// wikiWrites is an app encrypting what it stores with a secret of its own,
// and wikiReads the app reading it back.
func wikiWrites(t *testing.T, secret, plain string) []byte {
	t.Helper()
	key := sha256.Sum256([]byte(secret))
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil)
}

func wikiReads(secret string, data []byte) (string, error) {
	key := sha256.Sum256([]byte(secret))
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	return string(plain), err
}

func TestAppSecretsAgainstAnObjectStoreAndAVault(t *testing.T) {
	if os.Getenv("GENTIAN_LIVE_BUNDLE_SECRETS") == "" {
		t.Skip("set GENTIAN_LIVE_BUNDLE_SECRETS=1 to run an app's secrets through a MinIO server in docker and an OpenBao server")
	}
	ctx := context.Background()
	kv := startLiveBao(t)
	endpoint := startLiveMinio(t)
	random := func(context.Context) (secrets.Mode, error) { return secrets.ModeRandom, nil }
	seeder := secrets.NewSeeder(kv, secrets.NewDeriver("a master password", "a salt")).WithMode(random)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	store := &Store{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backup.MinIOAdminSecret, Namespace: layout.System("s3")},
		Data: map[string][]byte{"endpoint": []byte(endpoint),
			backup.DestinationAccessKeyField: []byte(liveMinioUser), backup.DestinationSecretKeyField: []byte(liveMinioPass)},
	}).Build()}
	ref := gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"}
	mc, err := store.Minio(ctx, &ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := mc.MakeBucket(ctx, ref.Bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}

	// The cluster's backup key: the public half encrypts, the private half
	// is the recovery kit's.
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	toBackupKey := backup.Encryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{identity.Recipient().String()}}
	key := Key{Identity: identity.String()}

	// --- installed: the app gets a secret, at random, and stores with it ---
	first, err := seeder.SeedAppSecret(ctx, "demo", "wiki", "encryption_key")
	if err != nil {
		t.Fatal(err)
	}
	derived := secrets.NewDeriver("a master password", "a salt").Derive(secrets.InternalPath("demo", "wiki", "encryption_key"), "value", 40)
	if first == derived {
		t.Fatal("the secret is the derived one: the mode is not random")
	}
	stored := wikiWrites(t, first, "a page of the wiki")

	// --- backed up ---
	value, found, err := seeder.ReadAppSecret(ctx, "demo", "wiki", "encryption_key")
	if err != nil || !found {
		t.Fatalf("read for the backup: %v, found %v", err, found)
	}
	sealed, err := backup.SealAppSecrets(toBackupKey, "", &bundle.AppSecrets{App: "wiki", Secrets: map[string]string{"encryption_key": value}})
	if err != nil {
		t.Fatal(err)
	}
	path := backup.SecretsArtefact("wiki")
	if err := store.PutArtefact(ctx, ref, path, sealed); err != nil {
		t.Fatal(err)
	}
	if err := store.PutArtefact(ctx, ref, "secrets/plain.json", []byte(`{"secrets":{"k":"v"}}`)); err == nil {
		t.Error("a plaintext artefact was written")
	}

	// What whoever reads the backup storage gets: the one object, under the
	// name of an encrypted file, with the value nowhere in it.
	var objects []string
	for obj := range mc.ListObjects(ctx, ref.Bucket, minio.ListObjectsOptions{Prefix: "nightly/", Recursive: true}) {
		objects = append(objects, obj.Key)
	}
	if len(objects) != 1 || objects[0] != "nightly/secrets/wiki.json.age" {
		t.Fatalf("the bucket holds %v", objects)
	}
	raw, err := ReadObject(ctx, mc, ref.Bucket, objects[0], 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("age-encryption.org/v1")) || bytes.Contains(raw, []byte(first)) {
		t.Fatal("the object in the bucket is not ciphertext")
	}
	// An ordinary age file: the age tool opens it with the backup key.
	if _, err := exec.LookPath("age"); err == nil {
		keyFile := filepath.Join(t.TempDir(), "identity.txt")
		if err := os.WriteFile(keyFile, []byte(identity.String()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("age", "-d", "-i", keyFile)
		cmd.Stdin = bytes.NewReader(raw)
		out, err := cmd.Output()
		if err != nil || !strings.Contains(string(out), `"app":"wiki"`) {
			t.Errorf("age -d: %v", err)
		}
	}

	// --- purged, and installed again ---
	if err := kv.DeleteTree(ctx, secrets.AppPath("demo", "wiki")); err != nil {
		t.Fatal(err)
	}
	second, err := seeder.SeedAppSecret(ctx, "demo", "wiki", "encryption_key")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("installed again, the app has the secret it had: nothing is shown")
	}
	if _, err := wikiReads(second, stored); err == nil {
		t.Fatal("the app reads its old data with its new secret")
	}

	// --- restored ---
	other, _ := age.GenerateX25519Identity()
	if _, err := store.AppSecrets(ctx, ref, path, Key{Identity: other.String()}); err == nil {
		t.Error("the secrets opened with a key that is not the backup key")
	}
	held, err := store.AppSecrets(ctx, ref, path, key)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := seeder.ReplaceAppSecret(ctx, "demo", "wiki", "encryption_key", held.Secrets["encryption_key"])
	if err != nil || !changed {
		t.Fatalf("replace: changed %v, %v", changed, err)
	}
	now, err := seeder.SeedAppSecret(ctx, "demo", "wiki", "encryption_key")
	if err != nil || now != first {
		t.Fatalf("after the restore the vault does not hold the value the data was written with: %v", err)
	}
	if plain, err := wikiReads(now, stored); err != nil || plain != "a page of the wiki" {
		t.Errorf("restored, the app cannot read what it stored before the backup: %v", err)
	}
	if changed, err := seeder.ReplaceAppSecret(ctx, "demo", "wiki", "encryption_key", first); err != nil || changed {
		t.Errorf("a second restore of the same bundle: changed %v, %v", changed, err)
	}

	// --- imported under a new name ---
	theirs, err := seeder.SeedAppSecret(ctx, "moved", "wiki", "encryption_key")
	if err != nil || theirs == first {
		t.Fatalf("the new tenant's own secret: %v", err)
	}
	if _, err := seeder.ReplaceAppSecret(ctx, "moved", "wiki", "encryption_key", held.Secrets["encryption_key"]); err != nil {
		t.Fatal(err)
	}
	record, err := kv.Get(ctx, secrets.InternalPath("moved", "wiki", "encryption_key"))
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := wikiReads(record["value"], stored); err != nil || plain != "a page of the wiki" {
		t.Errorf("imported under a new name, the app cannot read the data the bundle brought: %v", err)
	}

	// --- and under a passphrase the requester chose ---
	withPassphrase := backup.Encryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase}
	sealed, err = backup.SealAppSecrets(withPassphrase, "correct horse battery", &bundle.AppSecrets{App: "wiki", Secrets: map[string]string{"encryption_key": first}})
	if err != nil {
		t.Fatal(err)
	}
	own := gentianov1alpha1.BundleRef{Bucket: ref.Bucket, Prefix: "own-key"}
	if err := store.PutArtefact(ctx, own, path, sealed); err != nil {
		t.Fatal(err)
	}
	if got, err := store.AppSecrets(ctx, own, path, Key{Passphrase: "correct horse battery"}); err != nil || got.Secrets["encryption_key"] != first {
		t.Errorf("under a passphrase: %v", err)
	}
	if _, err := store.AppSecrets(ctx, own, path, key); err == nil {
		t.Error("a bundle under a passphrase opened with the backup key")
	}
}
