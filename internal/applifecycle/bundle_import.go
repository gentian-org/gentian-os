/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// Importing a bundle: the operator's three verbs under data-lifecycle.md §4
// (Import). UPLOAD takes a .gentian file and lays it out as a prefix in the
// cluster's own storage, so that every later step works on a bundle the
// capture Jobs could have written. INSPECT opens the manifest with the key
// the person supplied and says what the bundle holds. RESTORE starts a
// TenantRestore against it. Declaring the tenant from the manifest is the
// director's, because that is a commit.

// importBucket holds uploaded bundles until a restore of one has run to its
// end (bundlestore.ImportBucket, which says when an upload is removed).
const importBucket = bundlestore.ImportBucket

// Decryption is what a person supplies to open a bundle: one of the two.
type Decryption struct {
	Passphrase string `json:"passphrase,omitempty"`
	// Identity is an age identity (AGE-SECRET-KEY-1...), possibly several
	// lines of them.
	Identity string `json:"identity,omitempty"`
}

func (d Decryption) key() bundlestore.Key {
	return bundlestore.Key{Passphrase: d.Passphrase, Identity: d.Identity}
}

func (d Decryption) identities() ([]age.Identity, error) { return d.key().Identities() }

// Inspection is what inspect answers: the manifest and the cleartext header.
type Inspection struct {
	Bundle   gentianov1alpha1.BundleRef `json:"bundle"`
	Info     *backup.BundleInfo         `json:"info,omitempty"`
	Manifest *backup.Manifest           `json:"manifest"`
}

// UploadBundle lays a .gentian tar out under a fresh prefix in the import
// bucket and returns where it went, in one request: StartUpload and one
// FillUpload, with what was stored removed if it fails. A caller that has to
// survive a transfer stopping part-way starts the upload and fills it
// itself.
func (s *Service) UploadBundle(ctx context.Context, body io.Reader, tenant string) (*gentianov1alpha1.BundleRef, error) {
	ref, err := s.StartUpload(ctx, tenant)
	if err != nil {
		return nil, err
	}
	filled, err := s.FillUpload(ctx, body, tenant, ref.Prefix)
	if err != nil {
		if rerr := s.removeUpload(context.WithoutCancel(ctx), ref.Prefix); rerr != nil {
			return nil, fmt.Errorf("%w (and what was stored of it could not be removed: %v)", err, rerr)
		}
		return nil, err
	}
	return filled, nil
}

// errNoUpload is a prefix that is not an upload of this caller's, or, to
// fill, one already complete.
var errNoUpload = errors.New("no such upload being filled")

// errUploadStore is the import storage failing to store a file of an
// upload: worth sending the upload again, unlike a body that is not a
// bundle.
var errUploadStore = errors.New("the import storage did not store the file")

// StartUpload makes an empty upload under a fresh prefix of the import
// bucket, recorded as incomplete, and returns where: FillUpload fills it.
// tenant is the tenant whose administrator uploads it, empty for the
// cluster's: a tenant's upload is recorded as its own and counts towards its
// limits.
func (s *Service) StartUpload(ctx context.Context, tenant string) (*gentianov1alpha1.BundleRef, error) {
	mc, err := s.bundleClient(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return nil, err
	}
	if err := bundlestore.EnsureBucket(ctx, mc); err != nil {
		return nil, err
	}
	imports := bundlestore.ImportsOf(mc)
	if tenant != "" {
		if _, err := imports.TenantAllowance(ctx, tenant); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	prefix := now.Format("20060102-150405") + "-" + uuid.NewString()[:8]
	if err := imports.PutRecord(ctx, prefix, bundlestore.Record{Tenant: tenant, Kind: "upload", At: now, Incomplete: true}); err != nil {
		return nil, err
	}
	return &gentianov1alpha1.BundleRef{Bucket: importBucket, Prefix: prefix}, nil
}

// FillUpload lays a .gentian tar out under the prefix of an upload
// StartUpload made, and marks it complete once the tar has ended. A file
// already stored there at the size the tar gives is skipped: a transfer that
// stopped part-way is continued by sending the tar again, or only the files
// UploadedFiles does not list, and what arrived stays where it is. A tenant's
// upload is refused past its limits (bundlestore.MaxTenantUpload,
// MaxTenantImports), by the sizes the tar's own headers give before each file
// is stored, and what was stored of a refused one is removed.
func (s *Service) FillUpload(ctx context.Context, body io.Reader, tenant, prefix string) (*gentianov1alpha1.BundleRef, error) {
	mc, imports, rec, err := s.uploadOf(ctx, tenant, prefix)
	if err != nil {
		return nil, err
	}
	if !rec.Incomplete {
		return nil, fmt.Errorf("%w: upload %s is complete", errNoUpload, prefix)
	}
	allowance := int64(-1)
	if tenant != "" {
		if allowance, err = imports.TenantAllowanceFor(ctx, tenant, prefix); err != nil {
			return nil, err
		}
	}
	stored, err := imports.Stored(ctx, prefix)
	if err != nil {
		return nil, err
	}
	stopped := func(cause error) error {
		return fmt.Errorf("%w; what arrived of upload %s stays, and sending it again continues from there", cause, prefix)
	}
	tr := tar.NewReader(body)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, stopped(fmt.Errorf("the upload is not a bundle (tar), or it ended early: %w", err))
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if hdr.Typeflag != tar.TypeReg || name == "" || strings.Contains(name, "..") {
			continue
		}
		if total += hdr.Size; allowance >= 0 && total > allowance {
			cause := bundlestore.TooLarge(total, allowance)
			if rerr := imports.Remove(context.WithoutCancel(ctx), prefix); rerr != nil {
				return nil, fmt.Errorf("%w (and what was stored of it could not be removed: %v)", cause, rerr)
			}
			return nil, cause
		}
		if size, ok := stored[name]; ok && size == hdr.Size {
			continue
		}
		if _, err := mc.PutObject(ctx, importBucket, prefix+"/"+name, tr, hdr.Size, minio.PutObjectOptions{}); err != nil {
			return nil, stopped(fmt.Errorf("%w: %s: %v", errUploadStore, name, err))
		}
		stored[name] = hdr.Size
	}
	if len(stored) == 0 {
		return nil, errors.New("the upload holds no files")
	}
	rec.Incomplete = false
	if err := imports.PutRecord(ctx, prefix, *rec); err != nil {
		return nil, err
	}
	return &gentianov1alpha1.BundleRef{Bucket: importBucket, Prefix: prefix}, nil
}

// UploadedFiles is what an upload holds -- each file's size, by its name in
// the bundle -- and whether it is complete: a sender whose last fill's answer
// was lost learns here that it arrived.
func (s *Service) UploadedFiles(ctx context.Context, tenant, prefix string) (map[string]int64, bool, error) {
	_, imports, rec, err := s.uploadOf(ctx, tenant, prefix)
	if err != nil {
		return nil, false, err
	}
	files, err := imports.Stored(ctx, prefix)
	return files, !rec.Incomplete, err
}

// uploadOf is the upload at prefix, when it is one of tenant's (the
// cluster's for empty).
func (s *Service) uploadOf(ctx context.Context, tenant, prefix string) (*minio.Client, *bundlestore.Imports, *bundlestore.Record, error) {
	if !importPrefix.MatchString(prefix) {
		return nil, nil, nil, fmt.Errorf("%w: %q", errNoUpload, prefix)
	}
	mc, err := s.bundleClient(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return nil, nil, nil, err
	}
	imports := bundlestore.ImportsOf(mc)
	rec, err := imports.Record(ctx, prefix)
	if err != nil {
		return nil, nil, nil, err
	}
	if rec == nil || rec.Kind != "upload" || rec.Tenant != tenant {
		return nil, nil, nil, fmt.Errorf("%w: %q", errNoUpload, prefix)
	}
	return mc, imports, rec, nil
}

// removeUpload deletes an upload and its record.
func (s *Service) removeUpload(ctx context.Context, prefix string) error {
	mc, err := s.bundleClient(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return err
	}
	return bundlestore.ImportsOf(mc).Remove(ctx, prefix)
}

// uploadComplete refuses a bundle of the import bucket whose upload has not
// all arrived.
func (s *Service) uploadComplete(ctx context.Context, ref *gentianov1alpha1.BundleRef) error {
	if !bundlestore.IsImported(ref) {
		return nil
	}
	mc, err := s.bundleClient(ctx, ref)
	if err != nil {
		return err
	}
	rec, err := bundlestore.ImportsOf(mc).Record(ctx, ref.Prefix)
	if err != nil {
		return err
	}
	if rec != nil && rec.Incomplete {
		return fmt.Errorf("upload %s has not all arrived; send the rest of it first", ref.Prefix)
	}
	return nil
}

// InspectBundle opens the manifest with the key supplied and returns it.
func (s *Service) InspectBundle(ctx context.Context, bundle gentianov1alpha1.BundleRef, d Decryption) (*Inspection, error) {
	if bundle.Bucket == "" || bundle.Prefix == "" {
		return nil, errors.New("bundle.bucket and bundle.prefix are required")
	}
	if _, err := d.identities(); err != nil {
		return nil, err
	}
	mc, err := s.bundleClient(ctx, &bundle)
	if err != nil {
		return nil, err
	}
	out := &Inspection{Bundle: bundle}
	if raw, err := bundlestore.ReadObject(ctx, mc, bundle.Bucket, bundlestore.ObjectPrefix(bundle)+"bundle-info.json", 1<<20); err == nil {
		var info backup.BundleInfo
		if json.Unmarshal(raw, &info) == nil {
			out.Info = &info
		}
	}
	// The manifest by the one reader there is: the one a restore decides
	// by, so that what a person is shown is what would be restored.
	m, err := s.bundles().Manifest(ctx, bundle, d.key())
	if err != nil {
		return nil, err
	}
	out.Manifest = m
	return out, nil
}

// BackupBundle is where one of the tenant's own backups was written: its
// TenantExport's record, which names the destination it went to and the
// credential that opens it. Only a backup that finished has one.
func (s *Service) BackupBundle(ctx context.Context, tenantName, backupName string) (*gentianov1alpha1.BundleRef, error) {
	var export gentianov1alpha1.TenantExport
	if err := s.client.Get(ctx, types.NamespacedName{Name: backupName, Namespace: layout.Tenant(tenantName)}, &export); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("backup %q not found for tenant %q", backupName, tenantName)
		}
		return nil, err
	}
	if export.Status.Phase != gentianov1alpha1.TenantExportPhaseReady || export.Status.Bundle == nil {
		return nil, errBundleNotReady
	}
	return export.Status.Bundle, nil
}

// RestoreRequest starts a restore of a bundle into an existing tenant.
type RestoreRequest struct {
	Bundle gentianov1alpha1.BundleRef `json:"bundle"`
	// Backup names one of the tenant's own backups instead of a bundle: the
	// restore is then of that TenantExport (spec.exportRef), whose bundle is
	// found in the tenant's own namespace and nowhere else.
	Backup string `json:"backup,omitempty"`
	// Source names the bundle at a source instead (bundle_sources.go): one
	// of the cluster's, or one of the tenant's own. It is read where it is.
	Source     *SourceRef `json:"source,omitempty"`
	Decryption Decryption `json:"decryption"`
	Apps       []string   `json:"apps,omitempty"`
	// Name names the restore, for a caller that has to find it again: an
	// import decides it before it begins, so that after a restart it can ask
	// whether its restore was started and never starts a second. Empty names
	// it by the time.
	Name string `json:"name,omitempty"`
	// IntoNewTenant says the tenant was made new for this bundle: what an
	// import sets (TenantRestore spec.intoNewTenant). The data of apps the
	// bundle holds as uninstalled is then brought with stores made for it,
	// and the rights the bundle records as granted are not granted.
	IntoNewTenant bool `json:"intoNewTenant,omitempty"`
}

var restoreName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// RestoreStatus is one restore as the console reads it.
type RestoreStatus struct {
	Name                  string `json:"name"`
	Tenant                string `json:"tenant"`
	Phase                 string `json:"phase"`
	Message               string `json:"message,omitempty"`
	PasswordResetRequired bool   `json:"passwordResetRequired"`
	StartedAt             string `json:"startedAt,omitempty"`
	CompletedAt           string `json:"completedAt,omitempty"`
	// Complete is whether everything the bundle holds was put back. Set
	// when the restore has ended; false is explained by NotRestored.
	Complete *bool `json:"complete,omitempty"`
	// NotRestored names each app the bundle holds that was not put back,
	// and why.
	NotRestored []gentianov1alpha1.RestoreOmission `json:"notRestored,omitempty"`
	// Notes are what a restore does not bring back by design: stored
	// credentials a person entered, passwords, and what is said of this
	// restore in particular.
	Notes []string `json:"notes,omitempty"`
	// NameDerivation is "manifest", or "derived" for a bundle of format 1.
	NameDerivation string `json:"nameDerivation,omitempty"`
	// OutsideGuarantee says what makes this restore other than the round
	// trip the platform guarantees -- another version line, another domain
	// -- when something does: the message of the
	// restore's OutsideGuarantee condition.
	OutsideGuarantee string `json:"outsideGuarantee,omitempty"`
}

// StartRestore writes the key into a Secret, creates the TenantRestore
// naming it, and makes the restore its owner so the key goes when the
// restore does.
func (s *Service) StartRestore(ctx context.Context, tenantName string, req RestoreRequest, actor string) (*RestoreStatus, error) {
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	if req.Source != nil {
		if req.Bundle.Bucket != "" || req.Bundle.Prefix != "" || req.Backup != "" {
			return nil, errors.New("name a backup, a bundle or a source, one of them")
		}
		if req.Source.Tenant != "" && req.Source.Tenant != tenantName {
			return nil, fmt.Errorf("a source of tenant %s is restored into tenant %s alone", req.Source.Tenant, req.Source.Tenant)
		}
		ref, err := s.ResolveSource(ctx, *req.Source)
		if err != nil {
			return nil, err
		}
		req.Bundle = ref
	}
	byBackup := req.Backup != ""
	switch {
	case byBackup && (req.Bundle.Bucket != "" || req.Bundle.Prefix != ""):
		return nil, errors.New("name a backup or a bundle, not both")
	case byBackup:
		if _, err := s.BackupBundle(ctx, tenantName, req.Backup); err != nil {
			return nil, err
		}
	case req.Bundle.Bucket == "" || req.Bundle.Prefix == "":
		return nil, errors.New("bundle.bucket and bundle.prefix, or backup, are required")
	}
	if err := s.uploadComplete(ctx, &req.Bundle); err != nil {
		return nil, err
	}
	if _, err := req.Decryption.identities(); err != nil {
		return nil, err
	}
	ns := tenantNamespace(tenant)
	name := "restore-" + time.Now().UTC().Format("20060102-150405")
	if req.Name != "" {
		if !restoreName.MatchString(req.Name) {
			return nil, fmt.Errorf("name %q is not a name a restore can have: lower-case letters, digits and hyphens, forty at most", req.Name)
		}
		name = req.Name
	}
	restore := &gentianov1alpha1.TenantRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Labels:      map[string]string{"gentianos.io/tenant": tenantName},
			Annotations: map[string]string{"gentianos.io/requested-by": actor}},
		Spec: gentianov1alpha1.TenantRestoreSpec{
			Apps:          req.Apps,
			ConfirmTenant: tenantName,
			IntoNewTenant: req.IntoNewTenant,
		},
	}
	if byBackup {
		restore.Spec.ExportRef = req.Backup
	} else {
		restore.Spec.Bundle = &req.Bundle
	}
	// The key is stored first and named on the restore as it is created: the
	// operator takes a restore up the moment it exists, and one created
	// without its key was failed for want of it before a second write could
	// name it -- which then lost to the operator's status update.
	key, value := "identity", req.Decryption.Identity
	if req.Decryption.Passphrase != "" {
		key, value = "passphrase", req.Decryption.Passphrase
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-key", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{key: value},
	}
	if err := s.client.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("store the key: %w", err)
	}
	ref := &gentianov1alpha1.SecretKeyRef{Name: secret.Name, Key: key}
	if key == "passphrase" {
		restore.Spec.Decryption = &gentianov1alpha1.RestoreDecryption{PassphraseSecretRef: ref}
	} else {
		restore.Spec.Decryption = &gentianov1alpha1.RestoreDecryption{IdentitySecretRef: ref}
	}
	if err := s.client.Create(ctx, restore); err != nil {
		_ = s.client.Delete(ctx, secret)
		return nil, fmt.Errorf("create restore: %w", err)
	}
	// Owned by the restore, so the key goes when the restore does. Nothing
	// else writes the Secret, so this patch has nobody to lose to.
	owned := secret.DeepCopy()
	if err := controllerutil.SetControllerReference(restore, owned, s.client.Scheme()); err != nil {
		return nil, err
	}
	if err := s.client.Patch(ctx, owned, client.MergeFrom(secret)); err != nil {
		return nil, fmt.Errorf("restore %s started, but its key %s is not owned by it and stays when it goes: %w", name, secret.Name, err)
	}
	return restoreStatus(restore, tenantName), nil
}

// Restore reports one restore.
func (s *Service) Restore(ctx context.Context, tenantName, name string) (*RestoreStatus, error) {
	var restore gentianov1alpha1.TenantRestore
	if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: layout.Tenant(tenantName)}, &restore); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("restore %q not found for tenant %q", name, tenantName)
		}
		return nil, err
	}
	return restoreStatus(&restore, tenantName), nil
}

func restoreStatus(r *gentianov1alpha1.TenantRestore, tenant string) *RestoreStatus {
	out := &RestoreStatus{Name: r.Name, Tenant: tenant, Phase: string(r.Status.Phase),
		PasswordResetRequired: r.Status.PasswordResetRequired,
		Complete:              r.Status.Complete, NotRestored: r.Status.NotRestored,
		Notes: r.Status.Notes, NameDerivation: r.Status.NameDerivation}
	for _, c := range r.Status.Conditions {
		if c.Type == "OutsideGuarantee" {
			if c.Status == metav1.ConditionTrue {
				out.OutsideGuarantee = c.Message
			}
			continue
		}
		if c.Message != "" {
			out.Message = c.Message
		}
	}
	if r.Status.StartedAt != nil {
		out.StartedAt = r.Status.StartedAt.UTC().Format(time.RFC3339)
	}
	if r.Status.CompletedAt != nil {
		out.CompletedAt = r.Status.CompletedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *HTTPServer) registerImportRoutes(mux router) {
	mux.HandleFunc("POST /v1/bundles", h.handleBundleUpload)
	mux.HandleFunc("POST /v1/bundles/uploads", h.handleStartUpload)
	mux.HandleFunc("GET /v1/bundles/{prefix}/files", h.handleUploadedFiles)
	mux.HandleFunc("POST /v1/bundles/inspect", h.handleBundleInspect)
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/restore", h.handleRestore)
	// Where a restore stands is state, and the usher's to answer as well.
	mux.Read("GET /v1/tenants/{tenant}/restores/{name}", h.handleRestoreStatus)
}

func (h *HTTPServer) handleBundleUpload(w http.ResponseWriter, r *http.Request) {
	// An upload is as large as the bundle; the server's read deadline is for
	// requests that fit in memory.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	// tenant is the tenant whose administrator uploads, which the director
	// sets from its route: the upload counts towards that tenant's limits.
	// into is an upload StartUpload made, filled or continued; without it the
	// upload is made and filled at once.
	q := r.URL.Query()
	var ref *gentianov1alpha1.BundleRef
	var err error
	if into := q.Get("into"); into != "" {
		ref, err = h.Service.FillUpload(r.Context(), r.Body, q.Get("tenant"), into)
	} else {
		ref, err = h.Service.UploadBundle(r.Context(), r.Body, q.Get("tenant"))
	}
	switch {
	case errors.Is(err, bundlestore.ErrImportLimit):
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	case errors.Is(err, errNoUpload):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, errUploadStore):
		writeErr(w, http.StatusBadGateway, err)
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"bundle": ref})
}

func (h *HTTPServer) handleStartUpload(w http.ResponseWriter, r *http.Request) {
	ref, err := h.Service.StartUpload(r.Context(), r.URL.Query().Get("tenant"))
	switch {
	case errors.Is(err, bundlestore.ErrImportLimit):
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"bundle": ref})
}

func (h *HTTPServer) handleUploadedFiles(w http.ResponseWriter, r *http.Request) {
	files, complete, err := h.Service.UploadedFiles(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("prefix"))
	switch {
	case errors.Is(err, errNoUpload):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "complete": complete})
}

func (h *HTTPServer) handleBundleInspect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bundle gentianov1alpha1.BundleRef `json:"bundle"`
		// Tenant and Backup name one of a tenant's own backups instead of a
		// bundle, which is then the one its TenantExport records.
		Tenant string `json:"tenant,omitempty"`
		Backup string `json:"backup,omitempty"`
		// Source names a bundle at a source instead; the inspection answers
		// with the bundle it resolved to.
		Source     *SourceRef `json:"source,omitempty"`
		Decryption Decryption `json:"decryption"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if body.Source != nil && body.Backup != "" {
		writeErr(w, http.StatusBadRequest, errors.New("name a backup or a source, not both"))
		return
	}
	if body.Source != nil {
		ref, err := h.Service.ResolveSource(r.Context(), *body.Source)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		body.Bundle = ref
	}
	if body.Backup != "" {
		ref, err := h.Service.BackupBundle(r.Context(), body.Tenant, body.Backup)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		body.Bundle = *ref
	}
	out, err := h.Service.InspectBundle(r.Context(), body.Bundle, body.Decryption)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *HTTPServer) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req RestoreRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	res, err := h.Service.StartRestore(r.Context(), r.PathValue("tenant"), req, actorOf(r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (h *HTTPServer) handleRestoreStatus(w http.ResponseWriter, r *http.Request) {
	res, err := h.Service.Restore(r.Context(), r.PathValue("tenant"), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
