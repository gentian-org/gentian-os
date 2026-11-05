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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// OpenBaoSnapshots looks after the vault's daily snapshot
// (docs/guides/recovery.md §4) from the operator's side. The snapshot itself
// is taken by the CronJob openbao-snapshot in the vault's namespace
// (charts/gentian-os/templates/openbao-snapshots.yaml); what it needs from the
// operator, and what only the operator can say, is here:
//
//   - the store. A bucket in the platform's object store and a key that may
//     write to it alone, made by a bucket Job as an app's are
//     (backup.ObjectStorageProvisionContainerWith), kept in the vault at
//     gentian-os/kernel/openbao-snapshots/storage, which the job's
//     ExternalSecret reads;
//   - the signal. How old the newest snapshot in that bucket is, and whether
//     the vault is on Raft at all, written on the Cluster claim's status
//     (status.openbaoSnapshots) beside the platform's version, and as metrics.
//     That is what tells a person a snapshot is missing or stale; the job
//     cannot report its own absence.
//
// It reads the bucket rather than the CronJob's last success: a run that
// exited cleanly has not necessarily written a snapshot (a vault on file
// storage takes none), and the bucket is what a restore reads.
type OpenBaoSnapshots struct {
	// Client makes the bucket Job and its Secret in the object store's
	// namespace and reads the store's administrator Secret.
	Client client.Client
	// Reader reads the Cluster claims and their composites, uncached.
	Reader client.Reader
	// Seeder reaches the vault: the store's record, and which storage the
	// vault is on. Nil on an operator run without a vault, which then
	// reports the state Unknown.
	Seeder *secrets.Seeder
	// Bucket is where the snapshots are; the chart's openbaoSnapshots.bucket.
	Bucket string
	// StaleAfter is how old the newest snapshot may be before it is stale.
	StaleAfter time.Duration
	// Interval is how often the signal is written; ten minutes by default.
	Interval time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Snapshots lists the bucket; nil lists the cluster's own object store.
	Snapshots SnapshotLister
	// Storage says which storage the vault is on; nil asks the vault.
	Storage func(ctx context.Context) (string, error)
}

// The vault snapshot's names, shared with the chart and the documentation.
const (
	// OpenBaoSnapshotStorePath is the store's record in the vault.
	OpenBaoSnapshotStorePath = "gentian-os/kernel/openbao-snapshots/storage"
	// openBaoSnapshotBucketJob makes the bucket and its key.
	openBaoSnapshotBucketJob = "s3-bucket-kernel-openbao-snapshots"
	// openBaoSnapshotKeyAnnotation names the key a bucket Job made, by a
	// digest, so a key the vault holds anew is made again.
	openBaoSnapshotKeyAnnotation = "gentianos.io/key-id"
	defaultOpenBaoSnapshotStale  = 30 * time.Hour
	defaultOpenBaoSnapshotTick   = 10 * time.Minute
)

// The states status.openbaoSnapshots.state takes.
const (
	SnapshotCurrent = "Current"
	SnapshotStale   = "Stale"
	SnapshotMissing = "Missing"
	SnapshotNotRaft = "NotRaft"
	SnapshotUnknown = "Unknown"
)

var openBaoSnapshotStates = []string{SnapshotCurrent, SnapshotStale, SnapshotMissing, SnapshotNotRaft, SnapshotUnknown}

// SnapshotObject is one object of the snapshot bucket.
type SnapshotObject struct {
	Key          string
	LastModified time.Time
	Size         int64
}

// SnapshotLister lists the objects under a prefix of a bucket.
type SnapshotLister interface {
	List(ctx context.Context, bucket, prefix string) ([]SnapshotObject, error)
}

// SnapshotReport is what status.openbaoSnapshots says.
type SnapshotReport struct {
	// Instance is openbao or openbao-transit.
	Instance string `json:"instance"`
	// Storage is the vault's storage type, raft or file; empty when the
	// vault could not be asked.
	Storage string `json:"storage,omitempty"`
	// State is Current, Stale, Missing, NotRaft or Unknown.
	State string `json:"state"`
	// Newest is the newest snapshot's object key, and NewestAt when it was
	// written; AgeSeconds how old it is.
	Newest     string `json:"newest,omitempty"`
	NewestAt   string `json:"newestAt,omitempty"`
	AgeSeconds *int64 `json:"ageSeconds,omitempty"`
	// Count is how many snapshots the bucket holds.
	Count int `json:"count"`
	// Bucket is where they are, StaleAfter how old the newest may be.
	Bucket     string `json:"bucket"`
	StaleAfter string `json:"staleAfter"`
	// Message says what the state means and what to do.
	Message   string `json:"message"`
	CheckedAt string `json:"checkedAt"`
}

// NeedLeaderElection keeps one replica making Jobs and writing the status.
func (o *OpenBaoSnapshots) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It returns only when ctx ends.
func (o *OpenBaoSnapshots) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("openbao-snapshots")
	interval := o.Interval
	if interval <= 0 {
		interval = defaultOpenBaoSnapshotTick
	}
	for {
		for _, rep := range o.Once(ctx) {
			if rep.State != SnapshotCurrent {
				logger.Info("snapshots are not current", "instance", rep.Instance, "state", rep.State, "message", rep.Message)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// SnapshotInstance is one of the two OpenBao instances that are snapshotted.
type SnapshotInstance struct {
	// Name is the instance, the stem of its snapshots' names and their
	// prefix in the bucket: openbao or openbao-transit.
	Name string
	// Field is where its report is written on the Cluster claim's status.
	Field string
	// What it is, in a sentence, and the CronJob that snapshots it.
	What, Job string
	// Storage asks the instance which storage it is on; nil reads what its
	// job last found (the object storage.<raft|file> under its prefix).
	Storage func(ctx context.Context) (string, error)
}

// prefix is the instance's key prefix in the bucket.
func (i SnapshotInstance) prefix() string { return i.Name + "/" }

// snapshotName matches the instance's snapshot objects, without the prefix.
func (i SnapshotInstance) snapshotName() *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(i.Name) + `-[0-9]{8}T[0-9]{6}Z\.snap\.age$`)
}

// instances are the vault, asked directly, and the transit seal, which the
// operator does not reach and knows through its job's marker.
func (o *OpenBaoSnapshots) instances() []SnapshotInstance {
	return []SnapshotInstance{
		{Name: "openbao", Field: "openbaoSnapshots", What: "the vault",
			Job: "openbao-snapshot in the vault's namespace", Storage: o.storage},
		{Name: "openbao-transit", Field: "transitSnapshots", What: "the transit seal that unseals the vault",
			Job: "openbao-transit-snapshot in the seal's namespace"},
	}
}

// Once makes sure the store exists, judges each instance's newest snapshot
// and writes the reports. Every failure ends up in a report rather than an
// error: the report is what a person reads.
func (o *OpenBaoSnapshots) Once(ctx context.Context) []SnapshotReport {
	logger := log.FromContext(ctx).WithName("openbao-snapshots")
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	stale := o.StaleAfter
	if stale <= 0 {
		stale = defaultOpenBaoSnapshotStale
	}
	storeErr := o.EnsureStore(ctx)
	if storeErr != nil {
		logger.Error(storeErr, "the snapshot store could not be made; it is retried")
	}
	var reps []SnapshotReport
	for _, inst := range o.instances() {
		var storage string
		var storageErr error
		if inst.Storage != nil {
			storage, storageErr = inst.Storage(ctx)
			if storageErr != nil {
				logger.Info("which storage the instance is on could not be read", "instance", inst.Name, "error", storageErr.Error())
			}
		}
		objects, listErr := o.list(ctx, inst.prefix())
		rep := JudgeOpenBaoSnapshots(now(), stale, o.Bucket, inst, storage, storageErr, objects, listErr)
		if storeErr != nil && rep.State != SnapshotCurrent {
			rep.Message += " The store is not made yet: " + storeErr.Error()
		}
		recordOpenBaoSnapshotMetrics(rep)
		if err := writeClusterStatus(ctx, o.Reader, o.Client, inst.Field, rep); err != nil {
			logger.Error(err, "the snapshot report could not be written on the Cluster claim; it is retried", "instance", inst.Name)
		}
		reps = append(reps, rep)
	}
	return reps
}

// JudgeOpenBaoSnapshots is the report for what was found of one instance:
// the storage it said it is on, if it was asked, and the objects under its
// prefix in the bucket -- its snapshots, and the marker storage.<type> its
// job leaves, which answers for the storage where the instance was not asked
// or could not answer.
func JudgeOpenBaoSnapshots(now time.Time, staleAfter time.Duration, bucket string, inst SnapshotInstance,
	storage string, storageErr error, objects []SnapshotObject, listErr error) SnapshotReport {
	rep := SnapshotReport{
		Instance:   inst.Name,
		Bucket:     bucket,
		StaleAfter: staleAfter.String(),
		CheckedAt:  now.UTC().Format(time.RFC3339),
	}
	name := inst.snapshotName()
	var newest, marker *SnapshotObject
	for i := range objects {
		obj := &objects[i]
		rest := strings.TrimPrefix(obj.Key, inst.prefix())
		if rest == "storage.raft" || rest == "storage.file" {
			if marker == nil || obj.LastModified.After(marker.LastModified) {
				marker = obj
			}
			continue
		}
		if !name.MatchString(rest) {
			continue
		}
		rep.Count++
		if newest == nil || obj.LastModified.After(newest.LastModified) {
			newest = obj
		}
	}
	if storage == "" && marker != nil {
		storage = strings.TrimPrefix(strings.TrimPrefix(marker.Key, inst.prefix()), "storage.")
	}
	rep.Storage = storage
	if newest != nil {
		age := int64(now.Sub(newest.LastModified) / time.Second)
		rep.Newest = newest.Key
		rep.NewestAt = newest.LastModified.UTC().Format(time.RFC3339)
		rep.AgeSeconds = &age
	}

	what := inst.What
	switch {
	case storage != "" && storage != "raft":
		rep.State = SnapshotNotRaft
		rep.Message = fmt.Sprintf("%s is on %s storage, which has no consistent online snapshot, so none is taken. "+
			"Moving it to Raft is docs/guides/recovery.md §5.", upperFirst(what), storage)
	case listErr != nil:
		rep.State = SnapshotUnknown
		rep.Message = "The snapshots in bucket " + bucket + " could not be listed: " + listErr.Error()
	case newest == nil:
		rep.State = SnapshotMissing
		rep.Message = fmt.Sprintf("No snapshot of %s is in bucket %s. The CronJob %s writes one every day; its last Job's log says why it did not.",
			what, bucket, inst.Job)
	case now.Sub(newest.LastModified) > staleAfter:
		rep.State = SnapshotStale
		rep.Message = fmt.Sprintf("The newest snapshot of %s is %s old, more than %s. The last Job of the CronJob %s says why.",
			what, now.Sub(newest.LastModified).Round(time.Minute), staleAfter, inst.Job)
	default:
		rep.State = SnapshotCurrent
		rep.Message = fmt.Sprintf("The newest snapshot of %s is %s old.", what, now.Sub(newest.LastModified).Round(time.Minute))
	}
	if storageErr != nil && rep.State != SnapshotNotRaft {
		rep.Message += " Which storage it is on could not be read: " + storageErr.Error()
	}
	return rep
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// storage asks the vault which storage it is on: sys/seal-status, which
// answers without a token.
func (o *OpenBaoSnapshots) storage(ctx context.Context) (string, error) {
	if o.Storage != nil {
		return o.Storage(ctx)
	}
	if o.Seeder == nil || o.Seeder.KV() == nil {
		return "", errors.New("the operator runs without a vault")
	}
	status, body, err := o.Seeder.KV().Request(ctx, http.MethodGet, "sys/seal-status", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("sys/seal-status answered HTTP %d", status)
	}
	var seal struct {
		StorageType string `json:"storage_type"`
	}
	if err := json.Unmarshal(body, &seal); err != nil {
		return "", fmt.Errorf("sys/seal-status: %w", err)
	}
	if seal.StorageType == "" {
		return "", errors.New("sys/seal-status names no storage_type")
	}
	return seal.StorageType, nil
}

func (o *OpenBaoSnapshots) list(ctx context.Context, prefix string) ([]SnapshotObject, error) {
	if o.Snapshots != nil {
		return o.Snapshots.List(ctx, o.Bucket, prefix)
	}
	mc, err := (&bundlestore.Store{Client: o.Reader}).Minio(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return nil, err
	}
	return minioSnapshots{mc}.List(ctx, o.Bucket, prefix)
}

// minioSnapshots lists a bucket of the cluster's own store.
type minioSnapshots struct{ mc *minio.Client }

func (m minioSnapshots) List(ctx context.Context, bucket, prefix string) ([]SnapshotObject, error) {
	var out []SnapshotObject
	for obj := range m.mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			// A bucket not made yet holds no snapshot.
			if minio.ToErrorResponse(obj.Err).Code == "NoSuchBucket" {
				return nil, nil
			}
			return nil, obj.Err
		}
		out = append(out, SnapshotObject{Key: obj.Key, LastModified: obj.LastModified, Size: obj.Size})
	}
	return out, nil
}

// The markers stand alone: controller-gen skips one absorbed into a doc
// comment.
//
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// EnsureStore makes the snapshot bucket and its key, as an app's bucket is
// made: the key pair is seeded in the vault first, then a bucket Job in the
// object store's namespace makes the bucket and an identity that may read,
// write and list it alone, from a Secret owned by the Job and removed once
// it has run. The Job stays once it has succeeded, annotated with a digest
// of the key it made: that is the record the store exists, and a key the
// vault holds anew is made again.
func (o *OpenBaoSnapshots) EnsureStore(ctx context.Context) error {
	if o.Seeder == nil {
		return errors.New("the operator runs without a vault, and the key would have nowhere to be kept")
	}
	endpoint, err := o.storeEndpoint(ctx)
	if err != nil {
		return err
	}
	category, name := openBaoSnapshotStoreKey()
	creds, err := o.Seeder.SeedKernelS3(ctx, category, name, secrets.S3Creds{
		Endpoint: endpoint,
		Bucket:   o.Bucket,
		Region:   "us-east-1",
	})
	if err != nil {
		return err
	}
	if creds.Bucket == "" || creds.AccessKey == "" || creds.SecretKey == "" {
		return fmt.Errorf("the record at %s holds no bucket or key", OpenBaoSnapshotStorePath)
	}
	keyID := snapshotKeyID(creds.AccessKey)

	job := &batchv1.Job{}
	err = o.Client.Get(ctx, types.NamespacedName{Name: openBaoSnapshotBucketJob, Namespace: s3Namespace}, job)
	switch {
	case apierrors.IsNotFound(err):
		job = makeOpenBaoSnapshotBucketJob(creds.Bucket, keyID)
		if err := o.Client.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("make the snapshot bucket Job: %w", err)
		}
		return o.handKey(ctx, creds)
	case err != nil:
		return err
	case job.DeletionTimestamp != nil:
		return errors.New("the snapshot bucket Job is being removed; it is made again")
	case job.Annotations[openBaoSnapshotKeyAnnotation] != keyID || jobIsFailed(job):
		prop := metav1.DeletePropagationBackground
		if err := o.Client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove the snapshot bucket Job to run it again: %w", err)
		}
		return errors.New("the snapshot bucket Job failed or made another key; it is run again")
	case jobIsComplete(job):
		return deleteJobSecret(ctx, o.Client, job)
	default:
		return o.handKey(ctx, creds)
	}
}

// handKey writes the Secret the bucket Job reads the key pair from.
func (o *OpenBaoSnapshots) handKey(ctx context.Context, creds secrets.S3Creds) error {
	job := &batchv1.Job{}
	if err := o.Client.Get(ctx, types.NamespacedName{Name: openBaoSnapshotBucketJob, Namespace: s3Namespace}, job); err != nil {
		return err
	}
	if err := ensureJobSecret(ctx, o.Client, job, backup.ObjectStorageKeySecret(job, creds.AccessKey, creds.SecretKey)); err != nil {
		return err
	}
	return errors.New("the snapshot bucket Job is making the bucket")
}

// storeEndpoint is the object store's S3 address, from its administrator's
// Secret: what the job is told to write to.
func (o *OpenBaoSnapshots) storeEndpoint(ctx context.Context) (string, error) {
	secret := &corev1.Secret{}
	if err := o.Client.Get(ctx, types.NamespacedName{Name: storageAdminSecret, Namespace: s3Namespace}, secret); err != nil {
		return "", fmt.Errorf("the object store's address (%s/%s): %w", s3Namespace, storageAdminSecret, err)
	}
	endpoint := string(secret.Data["endpoint"])
	if endpoint == "" {
		return "", fmt.Errorf("%s/%s names no endpoint", s3Namespace, storageAdminSecret)
	}
	return endpoint, nil
}

// openBaoSnapshotStoreKey is OpenBaoSnapshotStorePath as the Seeder's
// category and name.
func openBaoSnapshotStoreKey() (category, name string) {
	rest := strings.TrimPrefix(OpenBaoSnapshotStorePath, "gentian-os/kernel/")
	category, name, _ = strings.Cut(rest, "/")
	return category, name
}

func snapshotKeyID(accessKey string) string {
	sum := sha256.Sum256([]byte(accessKey))
	return hex.EncodeToString(sum[:8])
}

// makeOpenBaoSnapshotBucketJob is the Job that makes the snapshot bucket and
// its identity, an app's bucket Job (makeS3BucketJob) for a bucket of the
// kernel's. No time-to-live: the succeeded Job is the record the store
// exists, and its Secret is removed once it has run.
func makeOpenBaoSnapshotBucketJob(bucket, keyID string) *batchv1.Job {
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	backoff := int32(3)
	container := backup.ObjectStorageProvisionContainerWith("create-bucket", bucket, openBaoSnapshotBucketJob, backup.ObjectStorageOptions{})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      openBaoSnapshotBucketJob,
			Namespace: s3Namespace,
			Labels: map[string]string{
				managedByLabel:                managedByValue,
				"app.kubernetes.io/component": "openbao-snapshot",
			},
			Annotations: map[string]string{openBaoSnapshotKeyAnnotation: keyID},
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: &deadline,
			BackoffLimit:          &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					managedByLabel: managedByValue,
				}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
	// The container administers the store through the master and the
	// filer, which admit this pod by its label and its client certificate.
	backup.ObjectStoreShellPod(&job.Spec.Template)
	return job
}

// writeClusterStatus writes one field of status on every Cluster claim's
// composite, as PlatformStatusWriter writes status.platform. A claim whose
// composite does not exist yet is skipped.
func writeClusterStatus(ctx context.Context, reader client.Reader, writer client.Client, field string, value any) error {
	return forEachClusterComposite(ctx, reader, func(xr *unstructured.Unstructured) error {
		patch, err := json.Marshal(map[string]any{"status": map[string]any{field: value}})
		if err != nil {
			return err
		}
		if err := writer.Status().Patch(ctx, xr, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return fmt.Errorf("write status.%s on %s: %w", field, xr.GetName(), err)
		}
		return nil
	})
}

// The vault's snapshot, as metrics: the state (1 for the one that holds) and
// the newest snapshot's age, -1 while there is none.
var (
	openBaoSnapshotState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gentianos_openbao_snapshot_state",
		Help: "The state of an OpenBao instance's snapshots (openbao, openbao-transit), 1 for the one that holds: Current, Stale, Missing, NotRaft or Unknown.",
	}, []string{"instance", "state"})
	openBaoSnapshotAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gentianos_openbao_snapshot_age_seconds",
		Help: "Age of the newest snapshot of an OpenBao instance (openbao, openbao-transit) in its bucket, in seconds; -1 while there is none.",
	}, []string{"instance"})
)

func recordOpenBaoSnapshotMetrics(rep SnapshotReport) {
	for _, s := range openBaoSnapshotStates {
		v := 0.0
		if s == rep.State {
			v = 1
		}
		openBaoSnapshotState.WithLabelValues(rep.Instance, s).Set(v)
	}
	if rep.AgeSeconds != nil {
		openBaoSnapshotAge.WithLabelValues(rep.Instance).Set(float64(*rep.AgeSeconds))
	} else {
		openBaoSnapshotAge.WithLabelValues(rep.Instance).Set(-1)
	}
}
