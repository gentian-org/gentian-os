/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The Jobs that move a tenant's objects, against an object store.
//
// What a bucket Job, a capture, an upload, a fetch and a restore do is
// decided by the store and by rclone, not by the text of a script. So this
// starts the server the platform deploys, in docker, and runs the Jobs this
// package builds -- every container of each, in order, with the image,
// command, environment and volumes the Job gives it and the Secrets it names
// filled in here -- and then asks the store what they did. It needs docker
// and the storage tools image (kernel/images/storage-tools, built locally and
// named by STORAGE_TOOLS_IMAGE), and is run when asked for:
//
//	scripts/tests/test-object-storage-lab.sh
//
// or, with the image built:
//
//	GENTIAN_LIVE_S3=1 STORAGE_TOOLS_IMAGE=<image> go test ./internal/backup -run TestObjectStorageJobsAgainstAServer -v

type liveStore struct {
	t       *testing.T
	net     string
	server  string
	secrets map[string]map[string]string
	volumes []string
}

// runJob runs every container of a Job, init containers first, each to its
// end, as the kubelet would: one scratch directory for the whole pod, the
// claims it mounts, and its Secrets as environment.
func (l *liveStore) runJob(job *batchv1.Job) (string, error) {
	l.t.Helper()
	spec := job.Spec.Template.Spec
	mounts := map[string]string{}
	for _, v := range spec.Volumes {
		name := fmt.Sprintf("%s-%s-%s", l.net, job.Name, v.Name)
		if v.PersistentVolumeClaim != nil {
			// A claim is the same disk to every Job that names it.
			name = l.net + "-pvc-" + v.PersistentVolumeClaim.ClaimName
		}
		l.volume(name)
		mounts[v.Name] = name
	}
	var log strings.Builder
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		out, err := l.runContainer(c, mounts)
		fmt.Fprintf(&log, "--- %s\n%s", c.Name, out)
		if err != nil {
			return log.String(), fmt.Errorf("%s/%s: %w", job.Name, c.Name, err)
		}
	}
	return log.String(), nil
}

// volume makes a docker volume that is writable by anybody, as an emptyDir is.
func (l *liveStore) volume(name string) {
	for _, v := range l.volumes {
		if v == name {
			return
		}
	}
	l.volumes = append(l.volumes, name)
	if out, err := exec.Command("docker", "run", "--rm", "-v", name+":/v", "alpine:3.22", "chmod", "0777", "/v").CombinedOutput(); err != nil {
		l.t.Fatalf("volume %s: %v\n%s", name, err, out)
	}
}

func (l *liveStore) runContainer(c corev1.Container, mounts map[string]string) (string, error) {
	args := []string{"run", "--rm", "--network", l.net}
	for _, m := range c.VolumeMounts {
		spec := mounts[m.Name] + ":" + m.MountPath
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	for _, e := range c.Env {
		value := e.Value
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			ref := e.ValueFrom.SecretKeyRef
			v, ok := l.secrets[ref.Name][ref.Key]
			if !ok {
				return "", fmt.Errorf("%s names %s/%s, which this test does not hold", e.Name, ref.Name, ref.Key)
			}
			value = v
		}
		args = append(args, "-e", e.Name+"="+value)
	}
	if sc := c.SecurityContext; sc != nil && sc.RunAsUser != nil {
		args = append(args, "--user", fmt.Sprint(*sc.RunAsUser))
	}
	words := append(append([]string{}, c.Command...), c.Args...)
	args = append(args, "--entrypoint", words[0], c.Image)
	args = append(args, words[1:]...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// keySecret holds a key pair in the Secret a bucket Job of that name reads
// it from, built by the code the operator builds it with.
func (l *liveStore) keySecret(job, accessKey, secretKey string) {
	sec := ObjectStorageKeySecret(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: job, Namespace: "system-s3"}}, accessKey, secretKey)
	l.secrets[sec.Name] = map[string]string{}
	for k, v := range sec.Data {
		l.secrets[sec.Name][k] = string(v)
	}
}

// pod runs containers as a Job of their own would.
func (l *liveStore) pod(name string, containers ...corev1.Container) (string, error) {
	return l.runJob(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: containers,
			Volumes:    []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		}}},
	})
}

// rclone runs rclone in the storage tools image with a key pair of its own.
func (l *liveStore) rclone(accessKey, secretKey string, stdin string, args ...string) (string, error) {
	endpoint := l.secrets[StorageAdminSecret]["endpoint"]
	cmd := exec.Command("docker", append([]string{"run", "--rm", "-i", "--network", l.net,
		"-e", "RCLONE_CONFIG_S_TYPE=s3", "-e", "RCLONE_CONFIG_S_PROVIDER=Other",
		"-e", "RCLONE_CONFIG_S_ENDPOINT=" + endpoint,
		"-e", "RCLONE_CONFIG_S_ACCESS_KEY_ID=" + accessKey, "-e", "RCLONE_CONFIG_S_SECRET_ACCESS_KEY=" + secretKey,
		"-e", "RCLONE_S3_NO_CHECK_BUCKET=true", "-e", "RCLONE_RETRIES=1", "-e", "RCLONE_LOW_LEVEL_RETRIES=1",
		kernel.StorageToolsImage(), "rclone"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (l *liveStore) admin(stdin string, args ...string) string {
	l.t.Helper()
	a := l.secrets[StorageAdminSecret]
	out, err := l.rclone(a["accessKey"], a["secretKey"], stdin, args...)
	if err != nil {
		l.t.Fatalf("rclone %v: %v\n%s", args, err, out)
	}
	return out
}

// objects lists a bucket or a prefix of one, recursively, as the
// administrator sees it: files only, sorted.
func (l *liveStore) objects(path string) []string {
	l.t.Helper()
	var files []string
	for _, line := range strings.Split(l.admin("", "lsf", "-R", "--files-only", "s:"+path), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files
}

func (l *liveStore) mustRun(job *batchv1.Job) string {
	l.t.Helper()
	out, err := l.runJob(job)
	if err != nil {
		l.t.Fatalf("%v\n%s", err, out)
	}
	return out
}

func startLiveStore(t *testing.T) *liveStore {
	t.Helper()
	if os.Getenv("GENTIAN_LIVE_S3") == "" {
		t.Skip("set GENTIAN_LIVE_S3=1 (and STORAGE_TOOLS_IMAGE) to run the object-storage Jobs against a server in docker")
	}
	l := &liveStore{t: t, net: fmt.Sprintf("gentian-s3-live-%d", os.Getpid())}
	if out, err := exec.Command("docker", "network", "create", l.net).CombinedOutput(); err != nil {
		t.Fatalf("docker network: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", l.server).Run()
		for _, v := range l.volumes {
			_ = exec.Command("docker", "volume", "rm", "-f", v).Run()
		}
		_ = exec.Command("docker", "network", "rm", l.net).Run()
	})
	l.server = l.net + "-server"
	admin, secret := "s3-admin", "live-admin-secret-key"
	l.startServer(admin, secret)
	l.secrets = map[string]map[string]string{
		StorageAdminSecret: {"endpoint": "http://" + l.server + ":9000", "master": l.server + ":9333", "accessKey": admin, "secretKey": secret},
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := l.rclone(admin, secret, "", "lsd", "s:"); err == nil {
			return l
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", "--tail", "30", l.server).CombinedOutput()
			t.Fatalf("the object store did not come up\n%s", out)
		}
		time.Sleep(time.Second)
	}
}

// startServer starts the S3 server the cluster runs, at the image the
// platform pins, with the arguments its chart gives it
// (kernel/services/infra-seaweedfs; charts/infra/seaweedfs all-in-one), as
// the user the chart runs it as, and with its administrator's identity in
// the file the chart mounts.
func (l *liveStore) startServer(accessKey, secretKey string) {
	l.t.Helper()
	dir := l.t.TempDir()
	config := fmt.Sprintf(`{"identities":[{"name":"storage_admin","credentials":[{"accessKey":%q,"secretKey":%q}],"actions":["Admin","Read","Write","List","Tagging"]}]}`, accessKey, secretKey)
	master := "[master.volume_growth]\ncopy_1 = 1\ncopy_2 = 2\ncopy_3 = 3\ncopy_other = 1\n"
	for name, body := range map[string]string{"seaweedfs_s3_config": config, "master.toml": master} {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		l.t.Fatal(err)
	}
	out, err := exec.Command("docker", "run", "-d", "--name", l.server, "--network", l.net, "--user", "1000:1000",
		"-v", dir+"/seaweedfs_s3_config:/etc/sw/s3/seaweedfs_s3_config:ro",
		"-v", dir+"/master.toml:/etc/seaweedfs/master.toml:ro",
		"--entrypoint", "/bin/sh", kernel.DefaultSeaweedFSImage, "-ec",
		"exec /usr/bin/weed -v=1 server -dir=/data -master -volume -ip=$(hostname -i) -ip.bind=0.0.0.0 -idleTimeout=30 "+
			"-volume.max=1000 -master.port=9333 -master.defaultReplication=000 -master.volumeSizeLimitMB=1000 "+
			"-volume.port=8080 -volume.readMode=proxy -volume.minFreeSpacePercent=1 -volume.compactionMBps=50 "+
			"-filer -filer.port=8888 -filer.dirListLimit=100000 -filer.defaultReplicaPlacement=000 "+
			"-s3 -s3.port=9000 -s3.config=/etc/sw/s3/seaweedfs_s3_config -s3.port.iceberg=0 -s3.port.lance=0 -s3.iam=false",
	).CombinedOutput()
	if err != nil {
		l.t.Fatalf("docker run: %v\n%s", err, out)
	}
}

// ageKey makes an age identity and its recipient.
func ageKey(t *testing.T) (identity, recipient string) {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", kernel.StorageToolsImage(), "age-keygen").CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			identity = line
		}
		if strings.HasPrefix(line, "# public key: ") {
			recipient = strings.TrimPrefix(line, "# public key: ")
		}
	}
	if identity == "" || recipient == "" {
		t.Fatalf("age-keygen said:\n%s", out)
	}
	return identity, recipient
}

func TestObjectStorageJobsAgainstAServer(t *testing.T) {
	l := startLiveStore(t)
	identity, recipient := ageKey(t)
	const (
		bucket   = "demo-wiki"
		appKey   = "demo-wiki-key"
		appSec   = "demo-wiki-secret-0123456789"
		staged   = "tx-nightly-vol-creds"
		keySec   = "trs-nightly-key"
		passSec  = "tx-nightly-passphrase"
		passText = "correct horse battery staple"
	)
	l.secrets[staged] = map[string]string{}
	for k, v := range l.secrets[StorageAdminSecret] {
		l.secrets[staged][k] = v
	}
	l.secrets[keySec] = map[string]string{"identity": identity, "passphrase": passText}
	l.secrets[passSec] = map[string]string{"passphrase": passText}

	// ── install: the bucket and the key the app reads it with ──────────────
	// The key pair reaches the Job in the Secret the operator makes beside
	// it, as the operator makes it.
	l.keySecret("s3-bucket-demo-wiki", appKey, appSec)
	provision := ObjectStorageProvisionContainer("create-bucket", bucket, "s3-bucket-demo-wiki")
	for _, e := range provision.Env {
		if strings.Contains(e.Value, appKey) || strings.Contains(e.Value, appSec) {
			t.Errorf("the bucket Job's spec carries the app's key in %s", e.Name)
		}
	}
	if out, err := l.pod("s3-bucket-demo-wiki", provision); err != nil {
		t.Fatalf("the bucket Job failed: %v\n%s", err, out)
	}
	if out, err := l.pod("s3-bucket-demo-wiki-again", provision); err != nil {
		t.Fatalf("the bucket Job run a second time failed: %v\n%s", err, out)
	}
	original := map[string]string{"a.txt": "alpha\n", "dir/b.txt": "bravo\n", "dir/sub/c.bin": strings.Repeat("c", 70000)}
	for key, body := range original {
		if out, err := l.rclone(appKey, appSec, body, "rcat", "s:"+bucket+"/"+key); err != nil {
			t.Fatalf("the app cannot write its own bucket: %v\n%s", err, out)
		}
	}

	// ── capture: a bucket, a volume and the manifest, in both encryptions ──
	for _, mode := range []gentianov1alpha1.ExportEncryptionMode{gentianov1alpha1.ExportEncryptionRecipient, gentianov1alpha1.ExportEncryptionPassphrase} {
		p := params()
		p.Namespace, p.Prefix = "system-s3", "nightly-"+string(mode)
		p.Encryption = Encryption{Mode: mode, Recipients: []string{recipient}}
		d := Decryption{Mode: mode, SecretName: keySec, SecretKey: "identity"}
		if mode == gentianov1alpha1.ExportEncryptionPassphrase {
			p.Encryption = Encryption{Mode: mode, PassphraseSecret: passSec, PassphraseKey: "passphrase"}
			d.SecretKey = "passphrase"
		}
		t.Run(string(mode), func(t *testing.T) {
			l.t = t
			p.Name = "tx-s3-" + string(mode)
			l.mustRun(S3ArchiveJob(p, bucket))

			// A volume's capture runs in the tenant's namespace, with the
			// staged copy of the credentials.
			vp := p
			vp.Name, vp.UploadCredentialsSecret = "tx-vol-"+string(mode), staged
			l.volume(l.net + "-pvc-data")
			if out, err := exec.Command("docker", "run", "--rm", "-v", l.net+"-pvc-data:/d", "alpine:3.22", "sh", "-c",
				"mkdir -p /d/files /d/cache && echo kept > /d/files/k.txt && echo derived > /d/cache/x").CombinedOutput(); err != nil {
				t.Fatalf("fill the claim: %v\n%s", err, out)
			}
			l.mustRun(VolumeArchiveJob(vp, "data", []string{"./cache"}))

			mp := p
			mp.Name = "tx-manifest-" + string(mode)
			manifest, err := ManifestJob(mp, &Manifest{SchemaVersion: ManifestSchemaVersion, Tenant: "demo"},
				NewBundleInfo("demo", p.Prefix, "now", p.Encryption))
			if err != nil {
				t.Fatal(err)
			}
			l.mustRun(manifest)

			got := l.objects(p.Bucket + "/" + p.Prefix)
			want := []string{"bundle-info.json", "manifest.json.age", "manifest.json.age.sha256",
				"s3/" + bucket + ".tar.gz.age", "s3/" + bucket + ".tar.gz.age.sha256",
				"volumes/data.tar.gz.age", "volumes/data.tar.gz.age.sha256"}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("the bundle holds %v, want %v", got, want)
			}
			// The checksum beside an artefact is one line, the hex digest of
			// the ciphertext and a newline: the format every bundle carries.
			for _, artefact := range []string{"manifest.json.age", "s3/" + bucket + ".tar.gz.age", "volumes/data.tar.gz.age"} {
				cipher := l.admin("", "cat", "s:"+p.Bucket+"/"+p.Prefix+"/"+artefact)
				sum := sha256.Sum256([]byte(cipher))
				if recorded := l.admin("", "cat", "s:"+p.Bucket+"/"+p.Prefix+"/"+artefact+".sha256"); recorded != hex.EncodeToString(sum[:])+"\n" {
					t.Errorf("%s.sha256 holds %q, want the digest of the ciphertext and a newline", artefact, recorded)
				}
				if !strings.HasPrefix(cipher, "age-encryption.org/v1\n") {
					t.Errorf("%s is not an age file", artefact)
				}
			}
			if info := l.admin("", "cat", "s:"+p.Bucket+"/"+p.Prefix+"/bundle-info.json"); !strings.Contains(info, `"howToDecrypt"`) {
				t.Errorf("bundle-info.json is not the plaintext header:\n%s", info)
			}

			// ── restore: the bucket changed since, and comes back as it was ──
			l.admin("extra\n", "rcat", "s:"+bucket+"/since.txt")
			l.admin("", "deletefile", "s:"+bucket+"/a.txt")
			l.admin("BRAVO\n", "rcat", "s:"+bucket+"/dir/b.txt")
			rp := p
			rp.Name = "trs-s3-" + string(mode)
			l.mustRun(S3RestoreJob(rp, d, S3Artefact(bucket), bucket, provision))
			if got := l.objects(bucket); strings.Join(got, ",") != "a.txt,dir/b.txt,dir/sub/c.bin" {
				t.Errorf("after the restore the bucket holds %v", got)
			}
			for key, body := range original {
				if out, err := l.rclone(appKey, appSec, "", "cat", "s:"+bucket+"/"+key); err != nil || out != body {
					t.Errorf("after the restore the app reads %s as %q (%v)", key, out, err)
				}
			}

			// A volume restore puts the archive back onto the claim.
			vr := vp
			vr.Name = "trs-vol-" + string(mode)
			if out, err := exec.Command("docker", "run", "--rm", "-v", l.net+"-pvc-data:/d", "alpine:3.22", "sh", "-c",
				"rm -rf /d/files").CombinedOutput(); err != nil {
				t.Fatalf("empty the claim: %v\n%s", err, out)
			}
			l.mustRun(VolumeRestoreJob(vr, d, VolumeArtefact("data"), "data"))
			if out, err := exec.Command("docker", "run", "--rm", "-v", l.net+"-pvc-data:/d", "alpine:3.22", "cat", "/d/files/k.txt").CombinedOutput(); err != nil || string(out) != "kept\n" {
				t.Errorf("the volume restore did not put files/k.txt back: %q %v", out, err)
			}

			// A wrong key is refused before anything is written.
			wrong := Decryption{Mode: d.Mode, SecretName: "wrong", SecretKey: d.SecretKey}
			other, _ := ageKey(t)
			l.secrets["wrong"] = map[string]string{"identity": other, "passphrase": "not it"}
			rp.Name = "trs-wrong-" + string(mode)
			if out, err := l.runJob(S3RestoreJob(rp, wrong, S3Artefact(bucket), bucket, provision)); err == nil {
				t.Errorf("a restore with the wrong key succeeded:\n%s", out)
			}
		})
		l.t = t
	}

	// ── a truncated artefact is refused by its checksum ─────────────────────
	p := params()
	p.Prefix, p.Name = "nightly-recipient", "trs-truncated"
	l.admin("", "copyto", "s:"+p.Bucket+"/nightly-recipient/s3/"+bucket+".tar.gz.age", "s:"+p.Bucket+"/kept.age")
	l.admin("age-encryption.org/v1\n", "rcat", "s:"+p.Bucket+"/nightly-recipient/s3/"+bucket+".tar.gz.age")
	out, err := l.runJob(S3RestoreJob(p, Decryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, SecretName: keySec, SecretKey: "identity"},
		S3Artefact(bucket), bucket, provision))
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Errorf("a truncated artefact was not refused by its checksum (%v):\n%s", err, out)
	}

	// ── deleting a bundle: that prefix, nothing beside it ───────────────────
	p.Prefix, p.Name = "nightly-recipient", "tx-delete"
	l.mustRun(BundleDeleteJob(p))
	if got := l.objects(p.Bucket + "/nightly-recipient"); len(got) != 0 {
		t.Errorf("the deleted bundle still holds %v", got)
	}
	if got := l.objects(p.Bucket + "/nightly-passphrase"); len(got) == 0 {
		t.Error("deleting one bundle took another whose name begins the same way")
	}
	if out := l.mustRun(BundleDeleteJob(p)); !strings.Contains(out, "nothing to delete") {
		t.Errorf("deleting a bundle that is gone did not say so:\n%s", out)
	}
	p.Bucket, p.Name = "demo-never-made", "tx-delete-nobucket"
	if out := l.mustRun(BundleDeleteJob(p)); !strings.Contains(out, "no bucket demo-never-made") {
		t.Errorf("deleting from a bucket that is not there did not say so:\n%s", out)
	}
	unreachable := params()
	unreachable.Name, unreachable.Endpoint, unreachable.UploadCredentialsSecret = "tx-delete-unreachable", "http://nothing-here.invalid:9000", "destination"
	l.secrets["destination"] = map[string]string{DestinationAccessKeyField: "x", DestinationSecretKeyField: "y"}
	if out, err := l.runJob(BundleDeleteJob(unreachable)); err == nil {
		t.Errorf("a store that cannot be reached was read as nothing to delete:\n%s", out)
	}

	// ── isolation: an app's key reaches its own bucket and nothing else ─────
	const (
		other    = "demo-files"
		otherKey = "demo-files-key"
		otherSec = "demo-files-secret-0123456789"
	)
	l.keySecret("s3-bucket-demo-files", otherKey, otherSec)
	if out, err := l.pod("s3-bucket-demo-files", ObjectStorageProvisionContainer("create-bucket", other, "s3-bucket-demo-files")); err != nil {
		t.Fatalf("the bucket Job failed: %v\n%s", err, out)
	}
	l.admin("files\n", "rcat", "s:"+other+"/f.txt")
	refused := func(what string, out string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("an app's key could %s:\n%s", what, out)
		} else if !strings.Contains(out, "AccessDenied") {
			t.Errorf("%s failed for another reason than the key's rights:\n%s", what, out)
		}
	}
	out, err = l.rclone(appKey, appSec, "", "cat", "s:"+other+"/f.txt")
	refused("read another app's bucket", out, err)
	out, err = l.rclone(appKey, appSec, "x\n", "rcat", "s:"+other+"/g.txt")
	refused("write into another app's bucket", out, err)
	out, err = l.rclone(appKey, appSec, "", "rmdir", "s:"+bucket)
	refused("delete its own bucket", out, err)
	out, err = l.rclone(appKey, appSec, "", "--s3-no-check-bucket=false", "mkdir", "s:demo-new-bucket")
	if err == nil && strings.Contains(l.admin("", "lsd", "s:"), " demo-new-bucket\n") {
		t.Errorf("an app's key made a bucket:\n%s", out)
	}
	if out, err := l.rclone(appKey, appSec, "", "lsd", "s:"); err != nil || strings.Contains(out, other) || !strings.Contains(out, bucket) {
		t.Errorf("an app's key lists more than its own bucket (%v):\n%s", err, out)
	}
	if out, err := l.rclone("", "", "", "lsf", "s:"+bucket); err == nil {
		t.Errorf("a request without a key could list a bucket:\n%s", out)
	}

	// ── purge: the app's bucket and its key go ──────────────────────────────
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	l.mustRun(ObjectStorageDestroyJob(tenant, "wiki", DestroyInTheBackground))
	if out, err := l.rclone(l.secrets[StorageAdminSecret]["accessKey"], l.secrets[StorageAdminSecret]["secretKey"], "", "lsd", "s:"); err != nil || strings.Contains(out, " "+bucket+"\n") {
		t.Errorf("the bucket is still listed after the purge (%v):\n%s", err, out)
	}
	if out, err := l.rclone(appKey, appSec, "", "lsd", "s:"); err == nil {
		t.Errorf("the app's key still works after the purge:\n%s", out)
	}
	if out := l.mustRun(ObjectStorageDestroyJob(tenant, "wiki", DestroyInTheBackground)); !strings.Contains(out, "bucket demo-wiki is not there") ||
		!strings.Contains(out, "identity demo-wiki is not there") {
		t.Errorf("a second purge did not say the bucket and the identity are gone:\n%s", out)
	}
	// The other app's bucket and key are untouched by the first's purge.
	if out, err := l.rclone(otherKey, otherSec, "", "cat", "s:"+other+"/f.txt"); err != nil || out != "files\n" {
		t.Errorf("purging one app's bucket touched another's (%v): %q", err, out)
	}
	// Nothing the Jobs printed holds a key: s3.configure answers with the
	// identity it wrote, secret and all.
	for name, job := range map[string]*batchv1.Job{
		"destroy": ObjectStorageDestroyJob(tenant, "files", DestroyInTheBackground),
	} {
		out := l.mustRun(job)
		if strings.Contains(out, otherSec) || strings.Contains(out, otherKey) {
			t.Errorf("the %s Job printed a key:\n%s", name, out)
		}
	}
	if out, err := l.pod("s3-bucket-demo-files-again", ObjectStorageProvisionContainer("create-bucket", other, "s3-bucket-demo-files")); err != nil || strings.Contains(out, otherSec) {
		t.Errorf("the bucket Job failed or printed the key (%v):\n%s", err, out)
	}
	// The unreachable store: the bucket Job fails, it does not wait forever
	// or report a bucket made.
	l.secrets[StorageAdminSecret]["master"] = "nothing-here.invalid:9333"
	if out, err := l.pod("s3-bucket-unreachable", ObjectStorageProvisionContainer("create-bucket", "demo-x", "")); err == nil {
		t.Errorf("the bucket Job succeeded against a store it cannot reach:\n%s", out)
	}
}
