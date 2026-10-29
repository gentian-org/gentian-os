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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The import bucket is cleaned up three ways (data-lifecycle.md §4): a bundle
// goes once the restore or import that read it has run (RemoveImported, from
// the restore controller); anything older than ImportMaxAge goes by the
// sweep below, whether it was used or not; and what a tenant's administrator
// puts there is bounded by MaxTenantUpload and MaxTenantImports.
//
// What was put there, when and for whom is a record beside the bundle, at
// RecordKey(prefix): an upload or a fetch writes it before the bundle's first
// byte, so even one that never finished is known and swept.

const (
	// ImportMaxAge is how long anything stays in the import bucket.
	ImportMaxAge = 7 * 24 * time.Hour
	// MaxTenantUpload bounds one bundle a tenant's administrator uploads or
	// fetches into the import bucket.
	MaxTenantUpload int64 = 20 << 30
	// MaxTenantImports bounds everything of one tenant's in the import
	// bucket at once: its uploads and its fetches together.
	MaxTenantImports int64 = 50 << 30
	// recordDir holds the records, one per prefix, beside the bundles.
	recordDir = ".records/"
)

// ErrImportLimit is a bundle the import bucket does not take for a tenant:
// larger than one may be, or more than the tenant may hold there.
var ErrImportLimit = errors.New("import storage limit")

// Record is what the import bucket knows of one prefix in it.
type Record struct {
	// Tenant is the tenant whose administrator put it there, and whose limit
	// it counts towards; empty for the cluster's administrator.
	Tenant string `json:"tenant,omitempty"`
	// Kind is upload or fetch.
	Kind string `json:"kind"`
	// At is when it was put there; the sweep goes by it.
	At time.Time `json:"at"`
	// Size is what a fetch was told the bundle is before it was copied; the
	// usage counts it until the copy is there.
	Size int64 `json:"size,omitempty"`
	// Incomplete is an upload whose files have not all arrived. It is filled
	// again until they have -- a transfer that stopped part-way continues
	// from the files already stored -- and nothing restores from it before.
	Incomplete bool `json:"incomplete,omitempty"`
}

// RecordKey is where a prefix's record is.
func RecordKey(prefix string) string { return recordDir + strings.Trim(prefix, "/") + ".json" }

// importObjects is the import bucket as cleaning it needs it. *minio.Client
// is one, through minioImports; the tests hold a map.
type importObjects interface {
	list(ctx context.Context, prefix string, recursive bool) ([]minio.ObjectInfo, error)
	read(ctx context.Context, key string) ([]byte, error)
	put(ctx context.Context, key string, data []byte) error
	remove(ctx context.Context, key string) error
}

type minioImports struct{ mc *minio.Client }

func (m minioImports) list(ctx context.Context, prefix string, recursive bool) ([]minio.ObjectInfo, error) {
	var out []minio.ObjectInfo
	for obj := range m.mc.ListObjects(ctx, ImportBucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: recursive}) {
		if obj.Err != nil {
			if minio.ToErrorResponse(obj.Err).Code == "NoSuchBucket" {
				return nil, nil
			}
			return nil, obj.Err
		}
		out = append(out, obj)
	}
	return out, nil
}

func (m minioImports) read(ctx context.Context, key string) ([]byte, error) {
	return ReadObject(ctx, m.mc, ImportBucket, key, 64<<10)
}

func (m minioImports) put(ctx context.Context, key string, data []byte) error {
	_, err := m.mc.PutObject(ctx, ImportBucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"})
	return err
}

func (m minioImports) remove(ctx context.Context, key string) error {
	return m.mc.RemoveObject(ctx, ImportBucket, key, minio.RemoveObjectOptions{})
}

// Imports is the import bucket of the cluster's own storage.
type Imports struct{ objects importObjects }

// ImportsOf is the import bucket reached through mc, which has to be a
// client of the cluster's own storage.
func ImportsOf(mc *minio.Client) *Imports { return &Imports{objects: minioImports{mc}} }

// OpenImports reaches the import bucket with the cluster's own keys.
func (s *Store) OpenImports(ctx context.Context) (*Imports, *minio.Client, error) {
	mc, err := s.Minio(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return nil, nil, err
	}
	return ImportsOf(mc), mc, nil
}

// EnsureBucket makes the import bucket when it is not there yet.
func EnsureBucket(ctx context.Context, mc *minio.Client) error {
	if err := mc.MakeBucket(ctx, ImportBucket, minio.MakeBucketOptions{}); err != nil {
		if exists, eerr := mc.BucketExists(ctx, ImportBucket); eerr != nil || !exists {
			return fmt.Errorf("import bucket: %w", err)
		}
	}
	return nil
}

// PutRecord writes a prefix's record.
func (im *Imports) PutRecord(ctx context.Context, prefix string, rec Record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := im.objects.put(ctx, RecordKey(prefix), raw); err != nil {
		return fmt.Errorf("record the import %s: %w", prefix, err)
	}
	return nil
}

// Record reads a prefix's record; nil when it has none.
func (im *Imports) Record(ctx context.Context, prefix string) (*Record, error) {
	raw, err := im.objects.read(ctx, RecordKey(prefix))
	if err != nil {
		if code := minio.ToErrorResponse(err).Code; code == "NoSuchKey" || code == "NoSuchBucket" {
			return nil, nil
		}
		return nil, err
	}
	var rec Record
	if json.Unmarshal(raw, &rec) != nil {
		return nil, nil
	}
	return &rec, nil
}

// Remove deletes everything under a prefix, and its record.
func (im *Imports) Remove(ctx context.Context, prefix string) error {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" || strings.HasPrefix(prefix+"/", recordDir) {
		return fmt.Errorf("refusing to remove %q from the import bucket", prefix)
	}
	objs, err := im.objects.list(ctx, prefix+"/", true)
	if err != nil {
		return fmt.Errorf("listing the uploaded bundle %s: %w", prefix, err)
	}
	for _, obj := range objs {
		if err := im.objects.remove(ctx, obj.Key); err != nil {
			return fmt.Errorf("removing %s: %w", obj.Key, err)
		}
	}
	if err := im.objects.remove(ctx, RecordKey(prefix)); err != nil && minio.ToErrorResponse(err).Code != "NoSuchKey" {
		return fmt.Errorf("removing the record of %s: %w", prefix, err)
	}
	return nil
}

// size is what a prefix holds, in bytes.
func (im *Imports) size(ctx context.Context, prefix string) (int64, error) {
	files, err := im.Stored(ctx, prefix)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, size := range files {
		n += size
	}
	return n, nil
}

// Stored is what a prefix holds: each file's size, by its name under the
// prefix. An object is stored whole or not at all, so a file listed here is
// one that arrived.
func (im *Imports) Stored(ctx context.Context, prefix string) (map[string]int64, error) {
	prefix = strings.Trim(prefix, "/") + "/"
	objs, err := im.objects.list(ctx, prefix, true)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(objs))
	for _, o := range objs {
		out[strings.TrimPrefix(o.Key, prefix)] = o.Size
	}
	return out, nil
}

// records reads every record, by prefix.
func (im *Imports) records(ctx context.Context) (map[string]Record, map[string]time.Time, error) {
	objs, err := im.objects.list(ctx, recordDir, true)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]Record{}
	written := map[string]time.Time{}
	for _, o := range objs {
		prefix := strings.TrimSuffix(strings.TrimPrefix(o.Key, recordDir), ".json")
		written[prefix] = o.LastModified
		raw, err := im.objects.read(ctx, o.Key)
		if err != nil {
			continue
		}
		var rec Record
		if json.Unmarshal(raw, &rec) == nil {
			out[prefix] = rec
		}
	}
	return out, written, nil
}

// TenantUsage is what a tenant holds in the import bucket: each prefix
// recorded as its own, at what it holds or, for a fetch still copying, at
// what the fetch was told it would hold.
func (im *Imports) TenantUsage(ctx context.Context, tenant string) (int64, error) {
	return im.tenantUsage(ctx, tenant, "")
}

// tenantUsage is TenantUsage without the prefix except.
func (im *Imports) tenantUsage(ctx context.Context, tenant, except string) (int64, error) {
	recs, _, err := im.records(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for prefix, rec := range recs {
		if rec.Tenant != tenant || prefix == except {
			continue
		}
		n, err := im.size(ctx, prefix)
		if err != nil {
			return 0, err
		}
		total += max(n, rec.Size)
	}
	return total, nil
}

// TenantAllowance is how much one more bundle of the tenant may hold:
// MaxTenantUpload, or what is left of MaxTenantImports when that is less.
// ErrImportLimit when nothing is left.
func (im *Imports) TenantAllowance(ctx context.Context, tenant string) (int64, error) {
	return im.TenantAllowanceFor(ctx, tenant, "")
}

// TenantAllowanceFor is TenantAllowance for the bundle at prefix, an upload
// being filled: what it already holds is its own, not counted against it a
// second time.
func (im *Imports) TenantAllowanceFor(ctx context.Context, tenant, prefix string) (int64, error) {
	used, err := im.tenantUsage(ctx, tenant, prefix)
	if err != nil {
		return 0, err
	}
	left := MaxTenantImports - used
	if left <= 0 {
		return 0, fmt.Errorf("%w: tenant %s holds %s in the import storage, the most a tenant may hold there; "+
			"what is there goes once it is restored, or after %d days", ErrImportLimit, tenant, GiB(used), int(ImportMaxAge.Hours()/24))
	}
	return min(left, MaxTenantUpload), nil
}

// TooLarge is the refusal of a bundle of size bytes against an allowance.
func TooLarge(size, allowance int64) error {
	if allowance < MaxTenantUpload {
		return fmt.Errorf("%w: the bundle is %s and the tenant may put only %s more into the import storage "+
			"(%s at most at once); what is there goes once it is restored, or after %d days",
			ErrImportLimit, GiB(size), GiB(allowance), GiB(MaxTenantImports), int(ImportMaxAge.Hours()/24))
	}
	return fmt.Errorf("%w: the bundle is %s and one bundle a tenant puts into the import storage may be %s at most",
		ErrImportLimit, GiB(size), GiB(MaxTenantUpload))
}

// GiB says a size in GiB, to one decimal.
func GiB(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30)) }

// importedAt is when a prefix was put into the bucket: its record's time,
// or the time its name carries (every upload and fetch names its prefix by
// the time), or, for neither, the newest of its objects.
func importedAt(prefix string, rec *Record, objs []minio.ObjectInfo) time.Time {
	if rec != nil && !rec.At.IsZero() {
		return rec.At
	}
	if len(prefix) >= 15 {
		if t, err := time.Parse("20060102-150405", prefix[:15]); err == nil {
			return t
		}
	}
	var newest time.Time
	for _, o := range objs {
		if o.LastModified.After(newest) {
			newest = o.LastModified
		}
	}
	return newest
}

// Sweep removes what has been in the import bucket longer than maxAge,
// except the prefixes inUse: a restore that has not ended still reads its
// bundle. Records whose prefix holds nothing go too, at the same age. It
// answers the prefixes it removed.
func (im *Imports) Sweep(ctx context.Context, now time.Time, maxAge time.Duration, inUse map[string]bool) ([]string, error) {
	top, err := im.objects.list(ctx, "", false)
	if err != nil {
		return nil, err
	}
	recs, written, err := im.records(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	seen := map[string]bool{}
	for _, o := range top {
		if !strings.HasSuffix(o.Key, "/") || o.Key == recordDir {
			continue
		}
		prefix := strings.TrimSuffix(o.Key, "/")
		seen[prefix] = true
		if inUse[prefix] {
			continue
		}
		var rec *Record
		if r, ok := recs[prefix]; ok {
			rec = &r
		}
		var objs []minio.ObjectInfo
		if rec == nil {
			if objs, err = im.objects.list(ctx, o.Key, true); err != nil {
				return removed, err
			}
		}
		at := importedAt(prefix, rec, objs)
		if at.IsZero() || now.Sub(at) < maxAge {
			continue
		}
		if err := im.Remove(ctx, prefix); err != nil {
			return removed, err
		}
		removed = append(removed, prefix)
	}
	for prefix, when := range written {
		if seen[prefix] || inUse[prefix] {
			continue
		}
		at := when
		if rec, ok := recs[prefix]; ok && !rec.At.IsZero() {
			at = rec.At
		}
		if now.Sub(at) < maxAge {
			continue
		}
		if err := im.objects.remove(ctx, RecordKey(prefix)); err != nil {
			return removed, fmt.Errorf("removing the record of %s: %w", prefix, err)
		}
		removed = append(removed, prefix)
	}
	return removed, nil
}

// Sweeper runs the sweep in the operator, once at start and then every
// Interval, on the leader alone.
type Sweeper struct {
	Store    *Store
	Client   client.Reader
	Interval time.Duration
	MaxAge   time.Duration
	Now      func() time.Time
}

// NeedLeaderElection is true: one replica sweeps.
func (s *Sweeper) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (s *Sweeper) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		s.SweepOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// SweepOnce sweeps once and logs what it did; a failure is retried at the
// next interval.
func (s *Sweeper) SweepOnce(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("import-sweep")
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	maxAge := s.MaxAge
	if maxAge <= 0 {
		maxAge = ImportMaxAge
	}
	inUse, err := ImportsInUse(ctx, s.Client)
	if err != nil {
		logger.Error(err, "listing the restores that read the import bucket")
		return
	}
	im, _, err := s.Store.OpenImports(ctx)
	if err != nil {
		logger.Error(err, "reaching the import bucket")
		return
	}
	removed, err := im.Sweep(ctx, now(), maxAge, inUse)
	if len(removed) > 0 {
		logger.Info("removed from the import bucket after their time", "prefixes", removed)
	}
	if err != nil {
		logger.Error(err, "sweeping the import bucket")
	}
}

// ImportsInUse are the prefixes of the import bucket a restore that has not
// ended reads.
func ImportsInUse(ctx context.Context, c client.Reader) (map[string]bool, error) {
	var restores gentianov1alpha1.TenantRestoreList
	if err := c.List(ctx, &restores); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range restores.Items {
		r := &restores.Items[i]
		if r.IsTerminal() {
			continue
		}
		for _, ref := range []*gentianov1alpha1.BundleRef{r.Spec.Bundle, r.Status.Bundle} {
			if IsImported(ref) {
				out[strings.Trim(ref.Prefix, "/")] = true
			}
		}
	}
	return out, nil
}
