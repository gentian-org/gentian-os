/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package bundlestore reaches a bundle where it sits in object storage, from
// inside the operator: to read its manifest, and to remove an uploaded one
// once it has been used.
//
// The capture and restore Jobs move a bundle's artefacts; nothing here does.
// What the operator itself has to read is the manifest, which is small and is
// what says what a bundle holds -- an inspection shows it to a person, and a
// restore decides by it what to put back. Both read it through this package,
// so they cannot be shown different things.
package bundlestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"filippo.io/age"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// ImportBucket holds uploaded bundles until a restore has read them. Not a
// tenant's backup bucket: the tenant does not exist yet when the upload
// arrives, and a bundle that turns out to be somebody else's must not land
// in a bucket a tenant can list.
//
// An upload stays here until a restore of it has run to its end, restored or
// failed, and is then removed (Store.RemoveImported). A restore that is
// refused before it changes anything leaves it, so the request can be made
// again with the right key.
const ImportBucket = "gentian-imports"

// Key is what opens a bundle: a passphrase, or an age identity
// (AGE-SECRET-KEY-1...), possibly several lines of them. One of the two.
type Key struct {
	Passphrase string
	Identity   string
}

// Identities are the age identities of a key.
func (k Key) Identities() ([]age.Identity, error) {
	switch {
	case k.Passphrase != "" && k.Identity != "":
		return nil, errors.New("give a passphrase or an identity, not both")
	case k.Passphrase != "":
		id, err := age.NewScryptIdentity(k.Passphrase)
		if err != nil {
			return nil, err
		}
		return []age.Identity{id}, nil
	case k.Identity != "":
		ids, err := age.ParseIdentities(strings.NewReader(k.Identity))
		if err != nil {
			return nil, fmt.Errorf("the identity does not parse: %w", err)
		}
		return ids, nil
	}
	return nil, errors.New("a passphrase or an age identity is required to open the bundle")
}

// Store reads bundles with the credentials the cluster holds for the store
// each one sits in.
type Store struct {
	// Client reads the credential Secrets, in the object store's namespace.
	Client client.Reader
}

// Minio reaches the store a bundle sits in, with the credentials the capture
// Jobs used. The platform's own MinIO records its address beside its keys; a
// configured destination has its address on the bundle and only the keys in
// the Secret -- the same asymmetry the capture Jobs' environment explains.
func (s *Store) Minio(ctx context.Context, ref *gentianov1alpha1.BundleRef) (*minio.Client, error) {
	secretName := ref.CredentialSecret
	if secretName == "" {
		secretName = backup.MinIOAdminSecret
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: layout.System("s3")}, secret); err != nil {
		return nil, fmt.Errorf("bundle credentials %q: %w", secretName, err)
	}
	endpoint := ref.Endpoint
	if endpoint == "" {
		endpoint = string(secret.Data["endpoint"])
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bundle endpoint %q is not a URL", endpoint)
	}
	return minio.New(u.Host, &minio.Options{
		Creds: credentials.NewStaticV4(string(secret.Data[backup.DestinationAccessKeyField]),
			string(secret.Data[backup.DestinationSecretKeyField]), ""),
		Secure: u.Scheme == "https",
		Region: ref.Region,
	})
}

// Manifest reads a bundle's manifest and opens it with the key.
func (s *Store) Manifest(ctx context.Context, ref gentianov1alpha1.BundleRef, key Key) (*bundle.Manifest, error) {
	if ref.Bucket == "" || ref.Prefix == "" {
		return nil, errors.New("bundle.bucket and bundle.prefix are required")
	}
	ids, err := key.Identities()
	if err != nil {
		return nil, err
	}
	mc, err := s.Minio(ctx, &ref)
	if err != nil {
		return nil, err
	}
	cipher, err := ReadObject(ctx, mc, ref.Bucket, ObjectPrefix(ref)+"manifest.json"+backup.EncryptedSuffix, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("the bundle has no manifest; an export that did not finish has none: %w", err)
	}
	return OpenManifest(cipher, ids)
}

// OpenManifest decrypts and reads a manifest, and refuses one of a format
// this build does not read.
func OpenManifest(cipher []byte, ids []age.Identity) (*bundle.Manifest, error) {
	plain, err := age.Decrypt(bytes.NewReader(cipher), ids...)
	if err != nil {
		return nil, fmt.Errorf("the manifest could not be opened with that key: %w", err)
	}
	var m bundle.Manifest
	if err := json.NewDecoder(plain).Decode(&m); err != nil {
		return nil, fmt.Errorf("the manifest does not parse: %w", err)
	}
	if m.SchemaVersion < bundle.OldestReadableSchemaVersion || m.SchemaVersion > bundle.SchemaVersion {
		return nil, fmt.Errorf("the bundle's manifest is of format %d and this platform reads formats %d to %d; "+
			"a bundle written by a newer platform has to be restored by one at least as new",
			m.SchemaVersion, bundle.OldestReadableSchemaVersion, bundle.SchemaVersion)
	}
	return &m, nil
}

// ObjectPrefix is the key prefix of a bundle's objects, ending in a slash.
func ObjectPrefix(ref gentianov1alpha1.BundleRef) string {
	return strings.TrimSuffix(ref.Prefix, "/") + "/"
}

// ReadObject reads one object, up to limit bytes.
func ReadObject(ctx context.Context, mc *minio.Client, bucket, key string, limit int64) ([]byte, error) {
	obj, err := mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = obj.Close() }()
	return io.ReadAll(io.LimitReader(obj, limit))
}

// IsImported reports whether a bundle is one that was uploaded to this
// cluster: in the import bucket of the platform's own storage.
func IsImported(ref *gentianov1alpha1.BundleRef) bool {
	return ref != nil && ref.Bucket == ImportBucket && ref.Endpoint == "" && ref.CredentialSecret == "" && ref.Prefix != ""
}

// RemoveImported deletes an uploaded bundle's objects. It refuses anything
// that is not an upload: a tenant's own bundles are deleted by deleting
// their export, and nothing else may reach them through here.
func (s *Store) RemoveImported(ctx context.Context, ref gentianov1alpha1.BundleRef) error {
	if !IsImported(&ref) {
		return fmt.Errorf("refusing to remove %s/%s: it is not an uploaded bundle", ref.Bucket, ref.Prefix)
	}
	mc, err := s.Minio(ctx, &ref)
	if err != nil {
		return err
	}
	prefix := ObjectPrefix(ref)
	for obj := range mc.ListObjects(ctx, ref.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("listing the uploaded bundle %s: %w", ref.Prefix, obj.Err)
		}
		if err := mc.RemoveObject(ctx, ref.Bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("removing %s: %w", obj.Key, err)
		}
	}
	return nil
}
