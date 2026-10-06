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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	"github.com/gentian-org/gentian-os/internal/layout"
)

// Importing a bundle: the operator's three verbs under sovereignty-concept.md
// §4.3. UPLOAD takes a .gentian file and lays it out as a prefix in the
// cluster's own storage, so that every later step works on a bundle the
// capture Jobs could have written. INSPECT opens the manifest with the key
// the person supplied and says what the bundle holds. RESTORE starts a
// TenantRestore against it. Declaring the tenant from the manifest is the
// director's, because that is a commit.

// importBucket holds uploaded bundles until a restore has read them. Not a
// tenant's backup bucket: the tenant does not exist yet when the upload
// arrives, and a bundle that turns out to be somebody else's must not land
// in a bucket a tenant can list.
const importBucket = "gentian-imports"

// Decryption is what a person supplies to open a bundle: one of the two.
type Decryption struct {
	Passphrase string `json:"passphrase,omitempty"`
	// Identity is an age identity (AGE-SECRET-KEY-1...), possibly several
	// lines of them.
	Identity string `json:"identity,omitempty"`
}

func (d Decryption) identities() ([]age.Identity, error) {
	switch {
	case d.Passphrase != "" && d.Identity != "":
		return nil, errors.New("give a passphrase or an identity, not both")
	case d.Passphrase != "":
		id, err := age.NewScryptIdentity(d.Passphrase)
		if err != nil {
			return nil, err
		}
		return []age.Identity{id}, nil
	case d.Identity != "":
		ids, err := age.ParseIdentities(strings.NewReader(d.Identity))
		if err != nil {
			return nil, fmt.Errorf("the identity does not parse: %w", err)
		}
		return ids, nil
	}
	return nil, errors.New("a passphrase or an age identity is required to open the bundle")
}

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
	ids, err := d.identities()
	if err != nil {
		return nil, err
	}
	mc, err := s.bundleClient(ctx, &bundle)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(bundle.Prefix, "/") + "/"
	out := &Inspection{Bundle: bundle}
	if raw, err := readObject(ctx, mc, bundle.Bucket, prefix+"bundle-info.json", 1<<20); err == nil {
		var info backup.BundleInfo
		if json.Unmarshal(raw, &info) == nil {
			out.Info = &info
		}
	}
	cipher, err := readObject(ctx, mc, bundle.Bucket, prefix+"manifest.json.age", 16<<20)
	if err != nil {
		return nil, fmt.Errorf("the bundle has no manifest; an export that did not finish has none: %w", err)
	}
	plain, err := age.Decrypt(bytes.NewReader(cipher), ids...)
	if err != nil {
		return nil, fmt.Errorf("the manifest could not be opened with that key: %w", err)
	}
	var m backup.Manifest
	if err := json.NewDecoder(plain).Decode(&m); err != nil {
		return nil, fmt.Errorf("the manifest does not parse: %w", err)
	}
	out.Manifest = &m
	return out, nil
}

func readObject(ctx context.Context, mc *minio.Client, bucket, key string, limit int64) ([]byte, error) {
	obj, err := mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(io.LimitReader(obj, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// RestoreRequest starts a restore of a bundle into an existing tenant.
type RestoreRequest struct {
	Bundle     gentianov1alpha1.BundleRef `json:"bundle"`
	Decryption Decryption                 `json:"decryption"`
	Apps       []string                   `json:"apps,omitempty"`
}

// RestoreStatus is one restore as the console reads it.
type RestoreStatus struct {
	Name                  string `json:"name"`
	Tenant                string `json:"tenant"`
	Phase                 string `json:"phase"`
	Message               string `json:"message,omitempty"`
	PasswordResetRequired bool   `json:"passwordResetRequired"`
	StartedAt             string `json:"startedAt,omitempty"`
	CompletedAt           string `json:"completedAt,omitempty"`
}

// StartRestore writes the key into a Secret the TenantRestore names, owned
// by it so the key goes when the restore does, and creates the restore.
func (s *Service) StartRestore(ctx context.Context, tenantName string, req RestoreRequest, actor string) (*RestoreStatus, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return nil, err
	}
	if req.Bundle.Bucket == "" || req.Bundle.Prefix == "" {
		return nil, errors.New("bundle.bucket and bundle.prefix are required")
	}
	if _, err := req.Decryption.identities(); err != nil {
		return nil, err
	}
	ns := layout.Tenant(tenantName)
	name := "restore-" + time.Now().UTC().Format("20060102-150405")
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
		PasswordResetRequired: r.Status.PasswordResetRequired}
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
