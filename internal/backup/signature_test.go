/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/api/bundle"
)

func signingKey(t *testing.T, seed string) *SigningKey {
	t.Helper()
	k, err := SigningKeyFromSeed(strings.Repeat(seed, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Every way a bundle can stand to the cluster it is restored on, and what
// the check says of each. Only one answer is Proven.
func TestProvenanceOfABundle(t *testing.T) {
	own, other := signingKey(t, "ab"), signingKey(t, "cd")
	m := &Manifest{SchemaVersion: ManifestSchemaVersion, Tenant: "demo", ClusterUID: "u-1",
		Rights: &bundle.ManifestRights{Granted: []bundle.RightsTuple{{User: "user:1", Relation: "admin", Object: "tenant:demo"}}}}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// As manifest.json holds it: the heredoc ends it with a newline.
	plain := append(append([]byte{}, encoded...), '\n')
	sig := own.SignManifest(encoded)
	tampered := []byte(strings.Replace(string(plain), `"user:1"`, `"user:2"`, 1))
	var parsedTampered Manifest
	if err := json.Unmarshal(tampered, &parsedTampered); err != nil {
		t.Fatal(err)
	}
	otherUID := *m
	otherUID.ClusterUID = "u-2"
	otherPlain, _ := json.Marshal(&otherUID)

	cases := []struct {
		name  string
		plain []byte
		m     *Manifest
		sig   *bundle.ManifestSignature
		key   *SigningKey
		uid   string
		want  string
	}{
		{"this cluster's bundle", plain, m, sig, own, "u-1", ProvenanceProven},
		{"this cluster has no key", plain, m, sig, nil, "u-1", ProvenanceNoKey},
		{"an unsigned bundle", plain, m, nil, own, "u-1", ProvenanceUnsigned},
		{"another cluster's key", plain, m, other.SignManifest(encoded), own, "u-1", ProvenanceOtherKey},
		{"a manifest changed after signing", tampered, &parsedTampered, sig, own, "u-1", ProvenanceBadSignature},
		{"a signature naming this key and made by another", plain, m,
			&bundle.ManifestSignature{Algorithm: "ed25519", PublicKey: own.PublicKey(), Signature: other.SignManifest(encoded).Signature}, own, "u-1", ProvenanceBadSignature},
		{"a damaged signature file", plain, m, &bundle.ManifestSignature{}, own, "u-1", ProvenanceBadSignature},
		{"another algorithm", plain, m, &bundle.ManifestSignature{Algorithm: "rsa", PublicKey: sig.PublicKey, Signature: sig.Signature}, own, "u-1", ProvenanceBadSignature},
		{"this cluster has no id", plain, m, sig, own, "", ProvenanceNoClusterID},
		{"signed here, taken on a cluster of another id", otherPlain, &otherUID, own.SignManifest(otherPlain), own, "u-1", ProvenanceOtherCluster},
	}
	for _, c := range cases {
		got := CheckProvenance(c.plain, c.m, c.sig, c.key, c.uid)
		if got.Reason != c.want || got.Message == "" {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)
		}
		if got.Proven() != (c.want == ProvenanceProven) {
			t.Errorf("%s: Proven() = %v", c.name, got.Proven())
		}
	}
	// Another key is named by its id, never by the key itself.
	if got := CheckProvenance(plain, m, other.SignManifest(encoded), own, "u-1"); !strings.Contains(got.Message, other.ID()) || !strings.Contains(got.Message, own.ID()) {
		t.Errorf("another key: %q", got.Message)
	}
}

// The seed is 32 bytes of hex, as the installer writes it; a file that is
// not there is no key, and an error never quotes the seed.
func TestSigningKeyFromFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := SigningKeyFromFile(filepath.Join(dir, "absent")); !errors.Is(err, ErrNoSigningKey) {
		t.Errorf("absent: %v", err)
	}
	bad := strings.Repeat("zz", 32)
	path := filepath.Join(dir, "signing_seed")
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SigningKeyFromFile(path); err == nil || strings.Contains(err.Error(), bad) {
		t.Errorf("not hex: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := SigningKeyFromFile(path)
	if err != nil || k.PublicKey() != signingKey(t, "ab").PublicKey() {
		t.Errorf("a seed with a newline: %v", err)
	}
}

// The manifest's Job writes the signature beside bundle-info.json, over the
// very bytes it stages the manifest with; without a key it writes none.
func TestManifestJobWritesTheSignature(t *testing.T) {
	key := signingKey(t, "ab")
	m := &Manifest{SchemaVersion: ManifestSchemaVersion, Tenant: "demo", ClusterUID: "u-1",
		Apps: []ManifestApp{{Name: "app\"one"}}}
	job, err := ManifestJob(params(), m, NewBundleInfo("demo", "nightly", "now", params().Encryption), key)
	if err != nil {
		t.Fatal(err)
	}
	staged := initContainerScript(job)
	manifest := regexp.MustCompile(`(?s)GENTIAN_MANIFEST_EOF' > \S+\n(.*?\n)GENTIAN_MANIFEST_EOF`).FindStringSubmatch(staged)
	info := strings.Join(containerByName(job, "bundle-info").Args, "\n")
	written := regexp.MustCompile(`(?s)GENTIAN_SIGNATURE_EOF' [^\n]*\n(.*?)\nGENTIAN_SIGNATURE_EOF`).FindStringSubmatch(info)
	if manifest == nil || written == nil || !strings.Contains(info, "/"+bundle.SignatureFile+`"`) {
		t.Fatalf("staged:\n%s\nbundle-info:\n%s", staged, info)
	}
	var sig bundle.ManifestSignature
	if err := json.Unmarshal([]byte(written[1]), &sig); err != nil {
		t.Fatal(err)
	}
	var read Manifest
	if err := json.Unmarshal([]byte(manifest[1]), &read); err != nil {
		t.Fatal(err)
	}
	if got := CheckProvenance([]byte(manifest[1]), &read, &sig, key, "u-1"); !got.Proven() {
		t.Errorf("the signature the Job writes does not hold over the manifest it stages: %+v", got)
	}

	unsigned, err := ManifestJob(params(), m, NewBundleInfo("demo", "nightly", "now", params().Encryption), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(containerByName(unsigned, "bundle-info").Args, "\n"), bundle.SignatureFile) {
		t.Error("an unsigned bundle is given a signature file")
	}
}
