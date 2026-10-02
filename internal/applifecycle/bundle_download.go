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

package applifecycle

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A bundle, downloaded: the artefacts under its S3 prefix streamed as one tar,
// which is the single-file container of sovereignty-concept.md §1.3. The
// artefacts are age-encrypted already, so what leaves here is what sits in
// the bucket; nothing is decrypted and nothing is re-packed.
//
// Streamed rather than staged: a bundle can be tens of gigabytes, and a copy
// made first would need somewhere to put it. The tar is written as the
// objects are read, and a reader that stops stops the reads.

// errBundleNotReady is a download asked for before the export finished: the
// manifest is written last, and a tar without it is not a bundle.
var errBundleNotReady = errors.New("the export has not finished; its bundle is not complete")

// bundleClient reaches the store a bundle sits in, with the credentials the
// capture Jobs used. The platform's own MinIO records its address beside its
// keys; a configured destination has its address on the bundle and only the
// keys in the Secret -- the same asymmetry backup.bundleEnv explains.
func (s *Service) bundleClient(ctx context.Context, bundle *gentianov1alpha1.BundleRef) (*minio.Client, error) {
	secretName := bundle.CredentialSecret
	if secretName == "" {
		secretName = backup.MinIOAdminSecret
	}
	secret := &corev1.Secret{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: layout.System("s3")}, secret); err != nil {
		return nil, fmt.Errorf("bundle credentials %q: %w", secretName, err)
	}
	endpoint := bundle.Endpoint
	if endpoint == "" {
		endpoint = string(secret.Data["endpoint"])
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bundle endpoint %q is not a URL", endpoint)
	}
	return minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(string(secret.Data[backup.DestinationAccessKeyField]), string(secret.Data[backup.DestinationSecretKeyField]), ""),
		Secure: u.Scheme == "https",
		Region: bundle.Region,
	})
}

// StreamBundle writes one export's bundle to w as a tar whose entries are the
// artefact names under the bundle's prefix.
func (s *Service) StreamBundle(ctx context.Context, tenantName, name string, w io.Writer) error {
	var export gentianov1alpha1.TenantExport
	if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: layout.Tenant(tenantName)}, &export); err != nil {
		return fmt.Errorf("export %q: %w", name, err)
	}
	if export.Status.Phase != gentianov1alpha1.TenantExportPhaseReady || export.Status.Bundle == nil {
		return errBundleNotReady
	}
	bundle := export.Status.Bundle
	mc, err := s.bundleClient(ctx, bundle)
	if err != nil {
		return err
	}
	prefix := strings.TrimSuffix(bundle.Prefix, "/") + "/"
	tw := tar.NewWriter(w)
	for obj := range mc.ListObjects(ctx, bundle.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("listing the bundle: %w", obj.Err)
		}
		rel := strings.TrimPrefix(obj.Key, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: obj.Size, ModTime: obj.LastModified}); err != nil {
			return err
		}
		body, err := mc.GetObject(ctx, bundle.Bucket, obj.Key, minio.GetObjectOptions{})
		if err != nil {
			return fmt.Errorf("reading %s: %w", obj.Key, err)
		}
		_, err = io.Copy(tw, body)
		_ = body.Close()
		if err != nil {
			return fmt.Errorf("streaming %s: %w", obj.Key, err)
		}
	}
	return tw.Close()
}

func (h *HTTPServer) handleBundleDownload(w http.ResponseWriter, r *http.Request) {
	tenant, name := r.PathValue("tenant"), r.PathValue("name")
	// Headers go out before the first byte, so the phase check is made
	// first and separately: a refusal has to be a JSON answer, not a
	// truncated tar.
	var export gentianov1alpha1.TenantExport
	if err := h.Service.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: layout.Tenant(tenant)}, &export); err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("backup %q not found for tenant %q", name, tenant))
		return
	}
	if export.Status.Phase != gentianov1alpha1.TenantExportPhaseReady || export.Status.Bundle == nil {
		writeErr(w, http.StatusConflict, errBundleNotReady)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.gentian"`, tenant, name))
	w.WriteHeader(http.StatusOK)
	if err := h.Service.StreamBundle(r.Context(), tenant, name, w); err != nil {
		// Too late for a status: the download is simply cut short, and the
		// log says why.
		ctrllog.FromContext(r.Context()).Error(err, "bundle download failed", "tenant", tenant, "export", name)
	}
}
