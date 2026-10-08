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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// Importing a bundle: the operator's three verbs under sovereignty-concept.md
// §4.3. UPLOAD takes a .gentian file and lays it out as a prefix in the
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
// bucket and returns where it went.
func (s *Service) UploadBundle(ctx context.Context, body io.Reader) (*gentianov1alpha1.BundleRef, error) {
	mc, err := s.bundleClient(ctx, &gentianov1alpha1.BundleRef{})
	if err != nil {
		return nil, err
	}
	if err := mc.MakeBucket(ctx, importBucket, minio.MakeBucketOptions{}); err != nil {
		if exists, eerr := mc.BucketExists(ctx, importBucket); eerr != nil || !exists {
			return nil, fmt.Errorf("import bucket: %w", err)
		}
	}
	prefix := time.Now().UTC().Format("20060102-150405") + "-" + uuid.NewString()[:8]
	tr := tar.NewReader(body)
	var count int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the upload is not a bundle (tar): %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if hdr.Typeflag != tar.TypeReg || name == "" || strings.Contains(name, "..") {
			continue
		}
		if _, err := mc.PutObject(ctx, importBucket, prefix+"/"+name, tr, hdr.Size, minio.PutObjectOptions{}); err != nil {
			return nil, fmt.Errorf("storing %s: %w", name, err)
		}
		count++
	}
	if count == 0 {
		return nil, errors.New("the upload holds no files")
	}
	return &gentianov1alpha1.BundleRef{Bucket: importBucket, Prefix: prefix}, nil
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

// RestoreRequest starts a restore of a bundle into an existing tenant.
type RestoreRequest struct {
	Bundle     gentianov1alpha1.BundleRef `json:"bundle"`
	Decryption Decryption                 `json:"decryption"`
	Apps       []string                   `json:"apps,omitempty"`
	// Name names the restore, for a caller that has to find it again: an
	// import decides it before it begins, so that after a restart it can ask
	// whether its restore was started and never starts a second. Empty names
	// it by the time.
	Name string `json:"name,omitempty"`
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
}

// StartRestore writes the key into a Secret the TenantRestore names, owned
// by it so the key goes when the restore does, and creates the restore.
func (s *Service) StartRestore(ctx context.Context, tenantName string, req RestoreRequest, actor string) (*RestoreStatus, error) {
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	if req.Bundle.Bucket == "" || req.Bundle.Prefix == "" {
		return nil, errors.New("bundle.bucket and bundle.prefix are required")
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
			Bundle:        &req.Bundle,
			Apps:          req.Apps,
			ConfirmTenant: tenantName,
		},
	}
	if err := s.client.Create(ctx, restore); err != nil {
		return nil, fmt.Errorf("create restore: %w", err)
	}
	key, value := "identity", req.Decryption.Identity
	if req.Decryption.Passphrase != "" {
		key, value = "passphrase", req.Decryption.Passphrase
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-key", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{key: value},
	}
	if err := controllerutil.SetControllerReference(restore, secret, s.client.Scheme()); err != nil {
		return nil, err
	}
	if err := s.client.Create(ctx, secret); err != nil {
		_ = s.client.Delete(ctx, restore)
		return nil, fmt.Errorf("store the key: %w", err)
	}
	ref := &gentianov1alpha1.SecretKeyRef{Name: secret.Name, Key: key}
	if key == "passphrase" {
		restore.Spec.Decryption = &gentianov1alpha1.RestoreDecryption{PassphraseSecretRef: ref}
	} else {
		restore.Spec.Decryption = &gentianov1alpha1.RestoreDecryption{IdentitySecretRef: ref}
	}
	if err := s.client.Update(ctx, restore); err != nil {
		return nil, fmt.Errorf("name the key on the restore: %w", err)
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
	mux.HandleFunc("POST /v1/bundles/inspect", h.handleBundleInspect)
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/restore", h.handleRestore)
	mux.HandleFunc("GET /v1/tenants/{tenant}/restores/{name}", h.handleRestoreStatus)
}

func (h *HTTPServer) handleBundleUpload(w http.ResponseWriter, r *http.Request) {
	// An upload is as large as the bundle; the server's read deadline is for
	// requests that fit in memory.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	ref, err := h.Service.UploadBundle(r.Context(), r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"bundle": ref})
}

func (h *HTTPServer) handleBundleInspect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bundle     gentianov1alpha1.BundleRef `json:"bundle"`
		Decryption Decryption                 `json:"decryption"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
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
