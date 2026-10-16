/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"context"
	"encoding/json"
	"errors"
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

// The seed is 32 bytes of hex at SigningKeyPath, as the installer writes it;
// a path that holds nothing is no key, and an error never quotes the seed.
func TestSigningKeyFromVault(t *testing.T) {
	absent := errors.New("not found")
	isAbsent := func(err error) bool { return errors.Is(err, absent) }
	vault := func(seed string, err error) func(context.Context, string) (map[string]string, error) {
		return func(_ context.Context, path string) (map[string]string, error) {
			if path != SigningKeyPath {
				t.Errorf("read %q, want %q", path, SigningKeyPath)
			}
			return map[string]string{"signing_seed": seed}, err
		}
	}
	ctx := context.Background()
	if _, err := SigningKeyFromVault(ctx, nil, isAbsent); !errors.Is(err, ErrNoSigningKey) {
		t.Errorf("no vault: %v", err)
	}
	if _, err := SigningKeyFromVault(ctx, vault("", absent), isAbsent); !errors.Is(err, ErrNoSigningKey) {
		t.Errorf("absent: %v", err)
	}
	if _, err := SigningKeyFromVault(ctx, vault("", errors.New("HTTP 403")), isAbsent); err == nil || errors.Is(err, ErrNoSigningKey) {
		t.Errorf("refused: %v", err)
	}
	bad := strings.Repeat("zz", 32)
	if _, err := SigningKeyFromVault(ctx, vault(bad, nil), isAbsent); err == nil || strings.Contains(err.Error(), bad) {
		t.Errorf("not hex: %v", err)
	}
	k, err := SigningKeyFromVault(ctx, vault(strings.Repeat("ab", 32)+"\n", nil), isAbsent)
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
