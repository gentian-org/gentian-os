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
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func sealed(t *testing.T, recipient age.Recipient, manifest any) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(w).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// A manifest of the format this build writes reads, a format 1 manifest
// still reads -- and is known for what it is -- and one from a newer platform
// is refused rather than half understood.
func TestOpenManifestReadsOldFormatsAndRefusesNewerOnes(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Identity: id.String()}
	ids, err := key.Identities()
	if err != nil {
		t.Fatal(err)
	}

	// Format 1, as it was written: apps with kinds, and no artefact named.
	v1 := []byte(`{"schemaVersion":1,"tenant":"old","export":"nightly","createdAt":"2026-01-01T00:00:00Z",
		"apps":[{"name":"wiki","chartVersion":"2.0.0","stores":[{"kind":"postgres","name":"wiki","path":""}]},
		        {"name":"gentian-tenant","stores":[{"kind":"identity","name":"gentian-tenant","path":""}]}],
		"identity":{"realm":"old","path":"identity/realm.tar.gz","passwordsIncluded":false},
		"shell":{"kind":"postgres","name":"old_shell","path":"postgres/old_shell.pgc"}}`)
	var raw map[string]any
	if err := json.Unmarshal(v1, &raw); err != nil {
		t.Fatal(err)
	}
	m, err := OpenManifest(sealed(t, id.Recipient(), raw), ids)
	if err != nil {
		t.Fatalf("a format 1 manifest no longer reads: %v", err)
	}
	if m.NamesArtefacts() || m.Apps[0].Name != "wiki" || m.Apps[0].Stores[0].Kind != "postgres" || m.Shell.Path != "postgres/old_shell.pgc" {
		t.Errorf("format 1 read as %+v", m)
	}

	// The format this build writes.
	now := bundle.Manifest{SchemaVersion: bundle.SchemaVersion, Tenant: "demo", Apps: []bundle.ManifestApp{{
		Name: "wiki", Digest: "sha256:aaa", DatabaseEngine: "postgresql", Releases: []string{"wiki-release"},
		Stores: []bundle.ManifestStore{{Kind: bundle.ArtefactVolume, Name: "wiki-release-data", Path: "volumes/wiki-release-data.tar.gz", Release: "wiki-release"}},
	}}}
	m, err = OpenManifest(sealed(t, id.Recipient(), now), ids)
	if err != nil {
		t.Fatal(err)
	}
	if !m.NamesArtefacts() || m.Apps[0].Stores[0].Release != "wiki-release" || m.Apps[0].Digest != "sha256:aaa" {
		t.Errorf("format %d read as %+v", bundle.SchemaVersion, m)
	}

	// Newer than this build, and older than anything.
	for _, v := range []int{bundle.SchemaVersion + 1, 0} {
		_, err := OpenManifest(sealed(t, id.Recipient(), bundle.Manifest{SchemaVersion: v}), ids)
		if err == nil || !strings.Contains(err.Error(), "format") {
			t.Errorf("format %d: err = %v, want a refusal", v, err)
		}
	}

	// The wrong key.
	other, _ := age.GenerateX25519Identity()
	if _, err := OpenManifest(sealed(t, other.Recipient(), now), ids); err == nil {
		t.Error("a manifest sealed to somebody else was opened")
	}
}

func TestAKeyIsAPassphraseOrAnIdentity(t *testing.T) {
	if _, err := (Key{}).Identities(); err == nil {
		t.Error("no key was accepted")
	}
	if _, err := (Key{Passphrase: "p", Identity: "AGE-SECRET-KEY-1X"}).Identities(); err == nil {
		t.Error("both were accepted")
	}
	if ids, err := (Key{Passphrase: "p"}).Identities(); err != nil || len(ids) != 1 {
		t.Errorf("passphrase: %v", err)
	}
}

// Only an upload is ever removed through here: a tenant's own bundles are
// deleted by deleting their export.
func TestOnlyAnUploadedBundleIsRemoved(t *testing.T) {
	upload := gentianov1alpha1.BundleRef{Bucket: ImportBucket, Prefix: "20261007-101500-1a2b3c4d"}
	if !IsImported(&upload) {
		t.Error("an upload is not recognised")
	}
	for _, ref := range []gentianov1alpha1.BundleRef{
		{Bucket: "demo-gentian-backup", Prefix: "nightly"},
		{Bucket: ImportBucket, Prefix: ""},
		{Bucket: ImportBucket, Prefix: "x", Endpoint: "https://elsewhere.example"},
		{Bucket: ImportBucket, Prefix: "x", CredentialSecret: "somebody-elses"},
	} {
		if IsImported(&ref) {
			t.Errorf("%+v is taken for an upload", ref)
		}
		// Refused before the store is ever reached: no client is needed to
		// say no.
		if err := (&Store{}).RemoveImported(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "not an uploaded bundle") {
			t.Errorf("%+v: err = %v, want a refusal", ref, err)
		}
	}
}
