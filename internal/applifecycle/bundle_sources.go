/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// Where bundles are read from, besides an upload: the cluster's declared
// sources (BundleSource), each S3 storage outside the cluster whose keys the
// custodian holds; and a tenant's own, the places its backups are written to.
//
// A caller names a bundle by its source and its prefix there, never by an
// address or a Secret: the platform finds the storage and the keys, so that
// nobody reads with a credential by naming it. Listing reads each bundle's
// plaintext header (bundle-info.json) and nothing else, and needs no key.
// Fetching copies a bundle into the import bucket of the cluster's own
// storage, by a Job confined to the source's address for as long as it runs.

// Tenant sources: the tenant's backup bucket in the cluster's own storage,
// and the external destination its backup policy names, when it names one.
const (
	TenantSourceStorage     = "storage"
	TenantSourceDestination = "destination"
)

// SourceRef names a bundle at a source. Tenant empty is one of the cluster's
// declared sources; set, one of that tenant's own.
type SourceRef struct {
	Tenant string `json:"tenant,omitempty"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// SourceView is a source as a person picks one: where it is, never its keys.
type SourceView struct {
	Name            string `json:"name"`
	Tenant          string `json:"tenant,omitempty"`
	Kind            string `json:"kind"` // declared | storage | destination
	Endpoint        string `json:"endpoint,omitempty"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	Region          string `json:"region,omitempty"`
	AddressingStyle string `json:"addressingStyle,omitempty"`
	Description     string `json:"description,omitempty"`
	// CredentialRequirement is set at the custodian; empty where the
	// cluster's own keys read it.
	CredentialRequirement string `json:"credentialRequirement,omitempty"`
	Ready                 bool   `json:"ready"`
	Message               string `json:"message,omitempty"`
}

// SourceBundle is one bundle at a source, as its plaintext header says.
type SourceBundle struct {
	Prefix string             `json:"prefix"`
	Info   *backup.BundleInfo `json:"info"`
}

// FetchStatus is one fetch.
type FetchStatus struct {
	Name   string    `json:"name"`
	Source SourceRef `json:"source"`
	// Phase is running, ready or failed.
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
	// Bundle is where the bundle is in the cluster's own storage, once
	// ready: an upload's place, which download, inspect, import and restore
	// take.
	Bundle      gentianov1alpha1.BundleRef `json:"bundle"`
	RequestedBy string                     `json:"requestedBy,omitempty"`
	StartedAt   string                     `json:"startedAt,omitempty"`
}

// sourcePrefix is one bundle below a source: one segment, as listed.
var sourcePrefix = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// importPrefix is a prefix of the import bucket, as an upload or a fetch
// names one.
var importPrefix = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{8}$`)

// listLimit bounds how many prefixes a listing reads a header of.
const listLimit = 500

var errNoSuchSource = errors.New("no such source")

// Sources lists the cluster's declared sources, by name.
func (s *Service) Sources(ctx context.Context) ([]SourceView, error) {
	var list gentianov1alpha1.BundleSourceList
	if err := s.client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list bundle sources: %w", err)
	}
	out := make([]SourceView, 0, len(list.Items))
	for i := range list.Items {
		src := &list.Items[i]
		v := SourceView{
			Name: src.Name, Kind: "declared", Endpoint: src.Spec.Endpoint, Bucket: src.Spec.Bucket,
			Prefix: src.Spec.Prefix, Region: src.Spec.Region, AddressingStyle: string(src.Spec.AddressingStyle),
			Description:           src.Spec.Description,
			CredentialRequirement: backup.SourceCredentialName(src.Name),
			Ready:                 src.Status.CredentialSatisfied,
			Message:               conditionMessage(src.Status.Conditions),
		}
		if v.Message == "" && !v.Ready {
			v.Message = "its keys are set at the custodian, as " + v.CredentialRequirement
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TenantSources lists a tenant's own: its backup bucket in the cluster's
// storage, and its backup policy's external destination when there is one.
func (s *Service) TenantSources(ctx context.Context, tenantName string) ([]SourceView, error) {
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	out := []SourceView{{Name: TenantSourceStorage, Tenant: tenantName, Kind: TenantSourceStorage,
		Bucket: backup.BackupBucket(tenant), Ready: true}}
	eff, err := s.tenantEffective(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if eff.Endpoint != "" {
		v := SourceView{Name: TenantSourceDestination, Tenant: tenantName, Kind: TenantSourceDestination,
			Endpoint: eff.Endpoint, Bucket: eff.Bucket, Region: eff.Region, AddressingStyle: string(eff.AddressingStyle),
			CredentialRequirement: eff.CredentialName}
		v.Ready, v.Message = s.policyCredentialReady(ctx, tenantName)
		out = append(out, v)
	}
	return out, nil
}

// tenantEffective is where the tenant's backups go, the cluster's policy
// and its own merged as the export controller merges them.
func (s *Service) tenantEffective(ctx context.Context, tenant *gentianov1alpha1.Tenant) (backup.Effective, error) {
	get := func(name string) (*gentianov1alpha1.BackupPolicy, error) {
		var p gentianov1alpha1.BackupPolicy
		if err := s.client.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &p, nil
	}
	cluster, err := get(backup.ClusterPolicyName)
	if err != nil {
		return backup.Effective{}, err
	}
	own, err := get(tenant.Name)
	if err != nil {
		return backup.Effective{}, err
	}
	if own != nil && (own.Spec.Scope != "tenant" || own.Spec.Tenant != tenant.Name) {
		own = nil
	}
	return backup.ResolveEffective(tenant, cluster, own)
}

// policyCredentialReady reads whether the keys the tenant's destination is
// written with are there, from the policy that resolved them.
func (s *Service) policyCredentialReady(ctx context.Context, tenantName string) (bool, string) {
	for _, name := range []string{tenantName, backup.ClusterPolicyName} {
		var p gentianov1alpha1.BackupPolicy
		if err := s.client.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
			continue
		}
		if p.Status.EffectiveEndpoint == "" {
			continue
		}
		return p.Status.CredentialSatisfied, conditionMessage(p.Status.Conditions)
	}
	return false, "the backup policy has not resolved its destination yet"
}

// sourceBase is where a source is, as a bundle reference whose prefix is
// the source's own: the place each bundle below it is read with.
func (s *Service) sourceBase(ctx context.Context, tenantName, name string) (gentianov1alpha1.BundleRef, error) {
	if tenantName == "" {
		var src gentianov1alpha1.BundleSource
		if err := s.client.Get(ctx, types.NamespacedName{Name: name}, &src); err != nil {
			if apierrors.IsNotFound(err) {
				return gentianov1alpha1.BundleRef{}, fmt.Errorf("%w: %q", errNoSuchSource, name)
			}
			return gentianov1alpha1.BundleRef{}, err
		}
		return backup.SourceBundle(&src, ""), nil
	}
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return gentianov1alpha1.BundleRef{}, err
	}
	switch name {
	case TenantSourceStorage:
		return gentianov1alpha1.BundleRef{Bucket: backup.BackupBucket(tenant)}, nil
	case TenantSourceDestination:
		eff, err := s.tenantEffective(ctx, tenant)
		if err != nil {
			return gentianov1alpha1.BundleRef{}, err
		}
		if eff.Endpoint == "" {
			return gentianov1alpha1.BundleRef{}, fmt.Errorf("%w: tenant %s writes its backups to the cluster's own storage", errNoSuchSource, tenantName)
		}
		return gentianov1alpha1.BundleRef{Bucket: eff.Bucket, Endpoint: eff.Endpoint, Region: eff.Region,
			AddressingStyle: eff.AddressingStyle, CredentialSecret: eff.CredentialSecret}, nil
	}
	return gentianov1alpha1.BundleRef{}, fmt.Errorf("%w: a tenant's sources are %s and %s", errNoSuchSource,
		TenantSourceStorage, TenantSourceDestination)
}

// ResolveSource is the bundle a source reference names.
func (s *Service) ResolveSource(ctx context.Context, ref SourceRef) (gentianov1alpha1.BundleRef, error) {
	if !sourcePrefix.MatchString(ref.Prefix) || ref.Prefix == "." || ref.Prefix == ".." {
		return gentianov1alpha1.BundleRef{}, fmt.Errorf("prefix %q is not one bundle below the source", ref.Prefix)
	}
	base, err := s.sourceBase(ctx, ref.Tenant, ref.Name)
	if err != nil {
		return gentianov1alpha1.BundleRef{}, err
	}
	base.Prefix = backup.JoinPrefix(base.Prefix, ref.Prefix)
	return base, nil
}

// SourceBundles lists the bundles at a source: each prefix directly below
// it whose header can be read. At a tenant's own source, only bundles of
// that tenant: a destination's bucket may hold other tenants' as well.
func (s *Service) SourceBundles(ctx context.Context, tenantName, name string) ([]SourceBundle, error) {
	base, err := s.sourceBase(ctx, tenantName, name)
	if err != nil {
		return nil, err
	}
	mc, err := s.bundleClient(ctx, &base)
	if err != nil {
		return nil, err
	}
	top := ""
	if base.Prefix != "" {
		top = bundlestore.ObjectPrefix(base)
	}
	out := []SourceBundle{}
	seen := 0
	// Cancelled on the way out: a listing left at its limit stops there.
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range mc.ListObjects(listCtx, base.Bucket, minio.ListObjectsOptions{Prefix: top}) {
		if obj.Err != nil {
			if minio.ToErrorResponse(obj.Err).Code == "NoSuchBucket" {
				return out, nil
			}
			return nil, fmt.Errorf("the source could not be listed: %w", obj.Err)
		}
		if !strings.HasSuffix(obj.Key, "/") {
			continue
		}
		if seen++; seen > listLimit {
			break
		}
		prefix := strings.TrimSuffix(strings.TrimPrefix(obj.Key, top), "/")
		if !sourcePrefix.MatchString(prefix) {
			continue
		}
		raw, err := bundlestore.ReadObject(ctx, mc, base.Bucket, obj.Key+"bundle-info.json", 1<<20)
		if err != nil {
			continue // not a bundle, or not a finished one
		}
		var info backup.BundleInfo
		if json.Unmarshal(raw, &info) != nil {
			continue
		}
		if tenantName != "" && info.Tenant != tenantName {
			continue
		}
		out = append(out, SourceBundle{Prefix: prefix, Info: &info})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Info.CreatedAt != out[j].Info.CreatedAt {
			return out[i].Info.CreatedAt > out[j].Info.CreatedAt
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out, nil
}

// errNotTenants is a bundle a tenant's administrator may not fetch: its
// header does not name their tenant.
var errNotTenants = errors.New("not a bundle of this tenant")

// StartFetch copies a bundle at a source into the import bucket of the
// cluster's own storage, by a Job in the object store's namespace.
//
// forTenant is the tenant whose administrator asks, empty for the cluster's.
// Theirs is a fetch from the tenant's own sources only, of a bundle whose
// header names the tenant, and the copy counts towards the tenant's limits
// in the import bucket, by the bundle's size at the source.
//
// The Job is made suspended, its way out is made and given to it, and only
// then is it let run: a pod of that namespace is otherwise free to go
// anywhere, and the policy is what confines it to the source.
func (s *Service) StartFetch(ctx context.Context, ref SourceRef, actor, forTenant string) (*FetchStatus, error) {
	if forTenant != "" && ref.Tenant != forTenant {
		return nil, fmt.Errorf("%w: a tenant's administrator fetches from the tenant's own sources", errNoSuchSource)
	}
	from, err := s.ResolveSource(ctx, ref)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	suffix := uuid.NewString()[:8]
	name := "bundle-fetch-" + now.Format("20060102-150405") + "-" + suffix
	target := now.Format("20060102-150405") + "-" + suffix
	record := bundlestore.Record{Tenant: forTenant, Kind: "fetch", At: now}
	if forTenant != "" {
		if record.Size, err = s.checkTenantFetch(ctx, from, forTenant); err != nil {
			return nil, err
		}
	}
	mc, err := s.bundleClient(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return nil, err
	}
	if err := bundlestore.EnsureBucket(ctx, mc); err != nil {
		return nil, err
	}
	if err := bundlestore.ImportsOf(mc).PutRecord(ctx, target, record); err != nil {
		return nil, err
	}
	ns := layout.System("s3")
	labels := map[string]string{}
	if ref.Tenant != "" {
		labels[meta.TenantLabel] = ref.Tenant
	}
	job, err := backup.BundleFetchJob(backup.FetchParams{
		Namespace: ns, Name: name, From: from, Bucket: bundlestore.ImportBucket, Prefix: target, Labels: labels,
	})
	if err != nil {
		return nil, err
	}
	job.Annotations = map[string]string{
		"gentianos.io/requested-by":   actor,
		"gentianos.io/source":         ref.Name,
		"gentianos.io/source-tenant":  ref.Tenant,
		"gentianos.io/source-prefix":  ref.Prefix,
		"gentianos.io/import-prefix":  target,
		"gentianos.io/source-address": from.Endpoint,
		"gentianos.io/import-tenant":  forTenant,
	}
	suspend := true
	job.Spec.Suspend = &suspend
	if err := s.client.Create(ctx, job); err != nil {
		return nil, fmt.Errorf("create the fetch: %w", err)
	}
	np, err := netpolicy.BundleFetchNetworkPolicy(ns, backup.FetchPolicyName(name), backup.FetchLabel, name, from.Endpoint)
	if err == nil {
		err = controllerutil.SetControllerReference(job, np, s.client.Scheme())
	}
	if err == nil {
		err = s.client.Create(ctx, np)
	}
	if err != nil {
		background := metav1.DeletePropagationBackground
		_ = s.client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &background})
		return nil, fmt.Errorf("confine the fetch to its source: %w", err)
	}
	patch := client.MergeFrom(job.DeepCopy())
	resume := false
	job.Spec.Suspend = &resume
	if err := s.client.Patch(ctx, job, patch); err != nil {
		return nil, fmt.Errorf("start the fetch: %w", err)
	}
	return fetchStatus(job), nil
}

// checkTenantFetch reads, at the source, whether a bundle is the tenant's by
// its header and whether it fits in what the tenant may still put into the
// import bucket, and answers its size there.
func (s *Service) checkTenantFetch(ctx context.Context, from gentianov1alpha1.BundleRef, tenant string) (int64, error) {
	src, err := s.bundleClient(ctx, &from)
	if err != nil {
		return 0, err
	}
	raw, err := bundlestore.ReadObject(ctx, src, from.Bucket, bundlestore.ObjectPrefix(from)+"bundle-info.json", 1<<20)
	var info backup.BundleInfo
	if err != nil || json.Unmarshal(raw, &info) != nil {
		return 0, fmt.Errorf("%w: the bundle's header could not be read, and a tenant's administrator fetches only a bundle whose header names the tenant", errNotTenants)
	}
	if info.Tenant != tenant {
		return 0, fmt.Errorf("%w: the bundle's header names tenant %q, and a tenant's administrator fetches only bundles of the tenant", errNotTenants, info.Tenant)
	}
	var size int64
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range src.ListObjects(listCtx, from.Bucket, minio.ListObjectsOptions{Prefix: bundlestore.ObjectPrefix(from), Recursive: true}) {
		if obj.Err != nil {
			return 0, fmt.Errorf("the source could not be listed: %w", obj.Err)
		}
		size += obj.Size
	}
	mc, err := s.bundleClient(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return 0, err
	}
	allowance, err := bundlestore.ImportsOf(mc).TenantAllowance(ctx, tenant)
	if err != nil {
		return 0, err
	}
	if size > allowance {
		return 0, bundlestore.TooLarge(size, allowance)
	}
	return size, nil
}

// Fetch reports one fetch. tenant, when set, is the tenant whose
// administrator asks: only a fetch made for that tenant is theirs to see.
func (s *Service) Fetch(ctx context.Context, name, tenant string) (*FetchStatus, error) {
	var job batchv1.Job
	if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: layout.System("s3")}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("no fetch %q; a finished one is forgotten after an hour", name)
		}
		return nil, err
	}
	if job.Labels[backup.FetchLabel] != name || (tenant != "" && job.Annotations["gentianos.io/import-tenant"] != tenant) {
		return nil, fmt.Errorf("no fetch %q", name)
	}
	return fetchStatus(&job), nil
}

func fetchStatus(job *batchv1.Job) *FetchStatus {
	a := job.Annotations
	st := &FetchStatus{
		Name:        job.Name,
		Source:      SourceRef{Tenant: a["gentianos.io/source-tenant"], Name: a["gentianos.io/source"], Prefix: a["gentianos.io/source-prefix"]},
		Phase:       "running",
		Message:     "copying the bundle into the cluster's storage",
		Bundle:      gentianov1alpha1.BundleRef{Bucket: bundlestore.ImportBucket, Prefix: a["gentianos.io/import-prefix"]},
		RequestedBy: a["gentianos.io/requested-by"],
		StartedAt:   job.CreationTimestamp.UTC().Format(time.RFC3339),
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			st.Phase, st.Message = "ready", "the bundle is in the cluster's storage"
		case batchv1.JobFailed:
			st.Phase = "failed"
			st.Message = "the bundle could not be copied: " + strings.TrimSpace(c.Reason+" "+c.Message) +
				". The fetch's log says what the source answered"
		}
	}
	return st
}

// errNoImported is a prefix the import bucket holds nothing under.
var errNoImported = errors.New("no such bundle in the cluster's import storage")

// importedRef is a bundle of the import bucket, when it holds one there.
// tenant, when set, is the tenant whose administrator asks: only a bundle
// recorded as that tenant's is theirs.
func (s *Service) importedRef(ctx context.Context, prefix, tenant string) (*gentianov1alpha1.BundleRef, error) {
	if !importPrefix.MatchString(prefix) {
		return nil, errNoImported
	}
	ref := &gentianov1alpha1.BundleRef{Bucket: bundlestore.ImportBucket, Prefix: prefix}
	mc, err := s.bundleClient(ctx, ref)
	if err != nil {
		return nil, err
	}
	if tenant != "" {
		rec, err := bundlestore.ImportsOf(mc).Record(ctx, prefix)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.Tenant != tenant {
			return nil, errNoImported
		}
	}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	obj, listed := <-mc.ListObjects(listCtx, ref.Bucket, minio.ListObjectsOptions{Prefix: bundlestore.ObjectPrefix(*ref), Recursive: true, MaxKeys: 1})
	switch {
	case !listed:
		return nil, errNoImported
	case obj.Err != nil && minio.ToErrorResponse(obj.Err).Code == "NoSuchBucket":
		return nil, errNoImported
	case obj.Err != nil:
		return nil, obj.Err
	}
	return ref, nil
}

func (h *HTTPServer) registerSourceRoutes(mux router) {
	mux.HandleFunc("GET /v1/bundle-sources", h.handleSources)
	mux.HandleFunc("GET /v1/bundle-sources/{name}/bundles", h.handleSourceBundles)
	mux.HandleFunc("GET /v1/tenants/{tenant}/bundle-sources", h.handleTenantSources)
	mux.HandleFunc("GET /v1/tenants/{tenant}/bundle-sources/{name}/bundles", h.handleSourceBundles)
	mux.HandleFunc("POST /v1/bundle-fetches", h.handleStartFetch)
	mux.HandleFunc("GET /v1/bundle-fetches/{name}", h.handleFetch)
	mux.HandleFunc("GET /v1/bundles/{prefix}/download", h.handleImportedDownload)
	mux.HandleFunc("POST /v1/bundle-sources/{name}/actions/retire", h.handleRetireSource)
}

// RetireSource deletes a source git no longer declares. The cluster's own
// declarations are synced without pruning (kernel/appsets/raw/11-claims.yaml),
// so the director, having committed the removal, asks for this. Its
// requirement, probe and Secret are owned by it and go with it.
func (s *Service) RetireSource(ctx context.Context, name string) error {
	src := &gentianov1alpha1.BundleSource{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := s.client.Delete(ctx, src); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete bundle source %s: %w", name, err)
	}
	return nil
}

func (h *HTTPServer) handleRetireSource(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.RetireSource(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "retired": true})
}

func (h *HTTPServer) handleSources(w http.ResponseWriter, r *http.Request) {
	out, err := h.Service.Sources(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

func (h *HTTPServer) handleTenantSources(w http.ResponseWriter, r *http.Request) {
	out, err := h.Service.TenantSources(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

func (h *HTTPServer) handleSourceBundles(w http.ResponseWriter, r *http.Request) {
	out, err := h.Service.SourceBundles(r.Context(), r.PathValue("tenant"), r.PathValue("name"))
	switch {
	case errors.Is(err, errNoSuchSource):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bundles": out})
}

func (h *HTTPServer) handleStartFetch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source SourceRef `json:"source"`
		// Tenant is the tenant whose administrator asks, which the director
		// sets from its route; empty for the cluster's administrator.
		Tenant string `json:"tenant,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	st, err := h.Service.StartFetch(r.Context(), body.Source, actorOf(r), body.Tenant)
	switch {
	case errors.Is(err, errNoSuchSource):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, bundlestore.ErrImportLimit):
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	case errors.Is(err, errNotTenants):
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (h *HTTPServer) handleFetch(w http.ResponseWriter, r *http.Request) {
	st, err := h.Service.Fetch(r.Context(), r.PathValue("name"), r.URL.Query().Get("tenant"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *HTTPServer) handleImportedDownload(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("prefix")
	ref, err := h.Service.importedRef(r.Context(), prefix, r.URL.Query().Get("tenant"))
	switch {
	case errors.Is(err, errNoImported):
		writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %q", err, prefix))
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.gentian"`, prefix))
	w.WriteHeader(http.StatusOK)
	if err := h.Service.streamRef(r.Context(), ref, w); err != nil {
		ctrllog.FromContext(r.Context()).Error(err, "imported bundle download failed", "prefix", prefix)
	}
}
