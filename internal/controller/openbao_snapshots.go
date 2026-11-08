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
//   - the store. A bucket in the platform's object store and, for each
//     instance, a key that may read, write and list its own prefix of it
//     alone (openbao/, openbao-transit/), made by a bucket Job as an app's
//     are (backup.ObjectStorageProvisionContainerWith) and kept in the vault
//     at gentian-os/kernel/openbao-snapshots/<instance>, which that
//     instance's job's ExternalSecret reads. Neither job can read or delete
//     the other's snapshots;
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
	// The one source of the name: the records the jobs read follow it.
	Bucket string
	// TransitDisabled says the chart takes no snapshot of the transit seal
	// (openbaoSnapshots.transit.enabled false): its store is not made and its
	// report says Disabled.
	TransitDisabled bool
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

	// pending is set while a store is being made, so the next pass comes
	// soon rather than a tick later.
	pending bool
}

// The vault snapshot's names, shared with the chart and the documentation.
const (
	// openBaoSnapshotStoreCategory holds each instance's store record in the
	// vault, gentian-os/kernel/openbao-snapshots/<instance>.
	openBaoSnapshotStoreCategory = "openbao-snapshots"
	// openBaoSnapshotKeyAnnotation names the record a bucket Job made its key
	// from and the bucket and prefix it scoped it to
	// (snapshotStoreFingerprint): a record written anew, or a bucket the chart
	// names anew, makes the Job run again.
	openBaoSnapshotKeyAnnotation = "gentianos.io/key-id"
	openBaoSnapshotRegion        = "us-east-1"
	defaultOpenBaoSnapshotStale  = 30 * time.Hour
	defaultOpenBaoSnapshotTick   = 10 * time.Minute
	// openBaoSnapshotPendingTick is the pass after one that made a store.
	openBaoSnapshotPendingTick = 30 * time.Second
)

// The states status.openbaoSnapshots.state takes.
const (
	SnapshotCurrent = "Current"
	SnapshotStale   = "Stale"
	SnapshotMissing = "Missing"
	SnapshotNotRaft = "NotRaft"
	SnapshotUnknown = "Unknown"
	// SnapshotDisabled is an instance the chart takes no snapshot of.
	SnapshotDisabled = "Disabled"
)

var openBaoSnapshotStates = []string{SnapshotCurrent, SnapshotStale, SnapshotMissing, SnapshotNotRaft, SnapshotUnknown, SnapshotDisabled}

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
	// State is Current, Stale, Missing, NotRaft, Unknown or Disabled.
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
			if rep.State != SnapshotCurrent && rep.State != SnapshotDisabled {
				logger.Info("snapshots are not current", "instance", rep.Instance, "state", rep.State, "message", rep.Message)
			}
		}
		next := interval
		if o.pending && openBaoSnapshotPendingTick < next {
			next = openBaoSnapshotPendingTick
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(next):
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
	// Disabled is an instance the chart takes no snapshot of.
	Disabled bool
}

// prefix is the instance's key prefix in the bucket.
func (i SnapshotInstance) prefix() string { return i.Name + "/" }

// StorePath is the instance's store record in the vault: the bucket, and the
// key that may write under the instance's prefix of it alone.
func (i SnapshotInstance) StorePath() string {
	return secrets.KernelPath(openBaoSnapshotStoreCategory, i.Name)
}

// bucketJob makes the bucket and the instance's key.
func (i SnapshotInstance) bucketJob() string { return "s3-bucket-kernel-" + i.Name + "-snapshots" }

// identity is the object store's name for the instance's key. An underscore,
// which no bucket name holds, keeps it apart from every identity named after
// a bucket.
func (i SnapshotInstance) identity() string { return "kernel_" + i.Name + "-snapshot" }

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
			Job: "openbao-transit-snapshot in the seal's namespace", Disabled: o.TransitDisabled},
	}
}

// Once makes sure each instance's store exists, judges its newest snapshot
// and writes the reports. Every failure ends up in a report rather than an
// error: the report is what a person reads. The object store's Secret is read
// once a pass, and the reports are written in one patch.
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
	lister, endpoint, storeErr := o.connect(ctx)
	o.pending = false
	var reps []SnapshotReport
	status := map[string]any{}
	for _, inst := range o.instances() {
		if inst.Disabled {
			rep := DisabledOpenBaoSnapshots(now(), stale, o.Bucket, inst)
			recordOpenBaoSnapshotMetrics(rep)
			status[inst.Field] = rep
			reps = append(reps, rep)
			continue
		}
		ready, err := false, storeErr
		if err == nil {
			ready, err = o.EnsureStore(ctx, inst, endpoint)
		}
		switch {
		case err != nil:
			logger.Error(err, "the snapshot store could not be made; it is retried", "instance", inst.Name)
		case !ready:
			o.pending = true
			logger.Info("making the snapshot store", "instance", inst.Name, "bucket", o.Bucket, "prefix", inst.prefix())
		}
		var storage string
		var storageErr error
		if inst.Storage != nil {
			storage, storageErr = inst.Storage(ctx)
			if storageErr != nil {
				logger.Info("which storage the instance is on could not be read", "instance", inst.Name, "error", storageErr.Error())
			}
		}
		var objects []SnapshotObject
		listErr := storeErr
		if lister != nil {
			objects, listErr = lister.List(ctx, o.Bucket, inst.prefix())
		}
		rep := JudgeOpenBaoSnapshots(now(), stale, o.Bucket, inst, storage, storageErr, objects, listErr)
		if rep.State != SnapshotCurrent {
			switch {
			case err != nil:
				rep.Message += " The snapshot store could not be made: " + err.Error()
			case !ready:
				rep.Message += " The operator is making the snapshot store."
			}
		}
		recordOpenBaoSnapshotMetrics(rep)
		status[inst.Field] = rep
		reps = append(reps, rep)
	}
	if err := writeClusterStatus(ctx, o.Reader, o.Client, status); err != nil {
		logger.Error(err, "the snapshot reports could not be written on the Cluster claim; they are retried")
	}
	return reps
}

// DisabledOpenBaoSnapshots is the report for an instance the chart takes no
// snapshot of.
func DisabledOpenBaoSnapshots(now time.Time, staleAfter time.Duration, bucket string, inst SnapshotInstance) SnapshotReport {
	return SnapshotReport{
		Instance:   inst.Name,
		State:      SnapshotDisabled,
		Bucket:     bucket,
		StaleAfter: staleAfter.String(),
		CheckedAt:  now.UTC().Format(time.RFC3339),
		Message: fmt.Sprintf("No snapshot of %s is taken: this cluster's values switch it off (openbaoSnapshots.transit.enabled). "+
			"Without one, no snapshot of the vault can be unsealed once the seal's volume is lost.", inst.What),
	}
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

// connect reads the object store's administrator Secret once: the endpoint
// the jobs are told to write to, and a lister of the bucket with the
// administrator's key. The lister is the injected one where there is one.
func (o *OpenBaoSnapshots) connect(ctx context.Context) (SnapshotLister, string, error) {
	mc, endpoint, err := (&bundlestore.Store{Client: o.Reader}).MinioAt(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		err = fmt.Errorf("the object store (%s/%s): %w", s3Namespace, storageAdminSecret, err)
	}
	switch {
	case o.Snapshots != nil:
		return o.Snapshots, endpoint, err
	case err != nil:
		return nil, "", err
	default:
		return minioSnapshots{mc}, endpoint, nil
	}
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

// EnsureStore makes the snapshot bucket and the instance's key, as an app's
// bucket is made: the key pair is seeded in the vault first, then a bucket
// Job in the object store's namespace makes the bucket and an identity that
// may read, write and list the instance's prefix of it alone, from a Secret
// owned by the Job and removed once it has run. The Job stays once it has
// succeeded, annotated with the record's metadata, the bucket and the prefix
// (snapshotStoreFingerprint): that is the record the store exists, and a
// record written anew or a bucket the chart names anew is made again.
//
// The record's address, bucket and region follow the operator's; only the
// key pair is kept once made. So a bucket changed in the chart reaches the
// jobs through their ExternalSecret, and the jobs write where the operator
// reads.
//
// ready is false while the store is being made, which is no error.
func (o *OpenBaoSnapshots) EnsureStore(ctx context.Context, inst SnapshotInstance, endpoint string) (ready bool, err error) {
	if o.Seeder == nil {
		return false, errors.New("the operator runs without a vault, and the key would have nowhere to be kept")
	}
	if endpoint == "" {
		return false, fmt.Errorf("%s/%s names no endpoint", s3Namespace, storageAdminSecret)
	}
	path := inst.StorePath()
	creds, err := o.Seeder.SeedS3(ctx, path, secrets.S3Creds{
		Endpoint: endpoint,
		Bucket:   o.Bucket,
		Region:   openBaoSnapshotRegion,
	})
	if err != nil {
		return false, err
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return false, fmt.Errorf("the record at %s holds no key", path)
	}
	if creds.Endpoint != endpoint || creds.Bucket != o.Bucket || creds.Region != openBaoSnapshotRegion {
		creds.Endpoint, creds.Bucket, creds.Region = endpoint, o.Bucket, openBaoSnapshotRegion
		if err := o.Seeder.Write(ctx, path, map[string]string{
			"endpoint":   creds.Endpoint,
			"bucket":     creds.Bucket,
			"region":     creds.Region,
			"access-key": creds.AccessKey,
			"secret-key": creds.SecretKey,
		}); err != nil {
			return false, fmt.Errorf("point the record at %s to bucket %s: %w", path, o.Bucket, err)
		}
	}
	record, err := o.Seeder.Metadata(ctx, path)
	if err != nil {
		return false, fmt.Errorf("the record at %s: %w", path, err)
	}
	keyID := snapshotStoreFingerprint(record, creds.Bucket, inst.prefix())

	// Read past the cache: a Job made a moment ago is there, and the key is
	// handed to it on this pass, not the next.
	name := types.NamespacedName{Name: inst.bucketJob(), Namespace: s3Namespace}
	job := &batchv1.Job{}
	err = o.Reader.Get(ctx, name, job)
	switch {
	case apierrors.IsNotFound(err):
		job = makeOpenBaoSnapshotBucketJob(inst, creds.Bucket, keyID)
		if err := o.Client.Create(ctx, job); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return false, fmt.Errorf("make the snapshot bucket Job %s: %w", name.Name, err)
			}
			if err := o.Reader.Get(ctx, name, job); err != nil {
				return false, err
			}
		}
		return false, o.handKey(ctx, job, creds)
	case err != nil:
		return false, err
	case job.DeletionTimestamp != nil:
		return false, nil
	case job.Annotations[openBaoSnapshotKeyAnnotation] != keyID || meta.JobFailed(job):
		prop := metav1.DeletePropagationBackground
		if err := o.Client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("remove the snapshot bucket Job %s to run it again: %w", name.Name, err)
		}
		if meta.JobFailed(job) {
			return false, fmt.Errorf("the snapshot bucket Job %s failed; it is run again", name.Name)
		}
		return false, nil
	case meta.JobComplete(job):
		return true, deleteJobSecret(ctx, o.Client, job)
	default:
		return false, o.handKey(ctx, job, creds)
	}
}

// handKey writes the Secret the bucket Job reads the key pair from, owned by
// the Job it is given.
func (o *OpenBaoSnapshots) handKey(ctx context.Context, job *batchv1.Job, creds secrets.S3Creds) error {
	return ensureJobSecret(ctx, o.Client, job, backup.ObjectStorageKeySecret(job, creds.AccessKey, creds.SecretKey))
}

// snapshotStoreFingerprint is what a bucket Job is annotated with: which
// record it made the key from, by when the vault made the record and the
// version it is at, and the bucket and prefix it scoped the key to. No part
// of the key: a record deleted and written again has a new created_time, and
// every write -- a key seeded anew, a bucket rewritten -- a new version.
func snapshotStoreFingerprint(record secrets.RecordMetadata, bucket, prefix string) string {
	return fmt.Sprintf("%s/v%d/%s/%s", record.CreatedTime, record.CurrentVersion, bucket, prefix)
}

// makeOpenBaoSnapshotBucketJob is the Job that makes the snapshot bucket and
// one instance's identity, scoped to its prefix: an app's bucket Job
// (makeS3BucketJob) for a bucket of the kernel's. No time-to-live: the
// succeeded Job is the record the store exists, and its Secret is removed
// once it has run.
func makeOpenBaoSnapshotBucketJob(inst SnapshotInstance, bucket, keyID string) *batchv1.Job {
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	backoff := int32(3)
	container := backup.ObjectStorageProvisionContainerWith("create-bucket", bucket, inst.bucketJob(),
		backup.ObjectStorageOptions{Prefix: inst.Name, Identity: inst.identity()})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inst.bucketJob(),
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

// writeClusterStatus writes fields of status on every Cluster claim's
// composite in one merge patch, as PlatformStatusWriter writes
// status.platform. A claim whose composite does not exist yet is skipped.
func writeClusterStatus(ctx context.Context, reader client.Reader, writer client.Client, fields map[string]any) error {
	patch, err := json.Marshal(map[string]any{"status": fields})
	if err != nil {
		return err
	}
	return forEachClusterComposite(ctx, reader, func(xr *unstructured.Unstructured) error {
		if err := writer.Status().Patch(ctx, xr, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return fmt.Errorf("write the snapshot reports on %s: %w", xr.GetName(), err)
		}
		return nil
	})
}

// The vault's snapshot, as metrics: the state (1 for the one that holds) and
// the newest snapshot's age, -1 while there is none.
var (
	openBaoSnapshotState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gentianos_openbao_snapshot_state",
		Help: "The state of an OpenBao instance's snapshots (openbao, openbao-transit), 1 for the one that holds: Current, Stale, Missing, NotRaft, Unknown or Disabled.",
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
