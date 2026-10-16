/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gentian-org/gentian-os/api/bundle"
)

// A bundle's manifest is signed by the cluster that wrote it, so that a
// restore can tell a bundle this cluster wrote from one it did not. Only
// such a bundle, of a tenant of the same name, brings back the rights that
// were granted in the tenant (internal/controller/tenant_rights.go).
//
// The key is the cluster's own: 32 random bytes the installer writes once
// to the vault (gentian-os/kernel/backup/signing), which the recovery kit
// carries so that a cluster rebuilt from the kit keeps it. It signs bundle
// manifests and nothing else.

// ErrNoSigningKey is a cluster that holds no bundle signing key.
var ErrNoSigningKey = errors.New("this cluster holds no bundle signing key")

// SigningKey is the cluster's bundle signing key pair.
type SigningKey struct {
	private ed25519.PrivateKey
}

// SigningKeyFromSeed derives the pair from the 32-byte seed, as the 64 hex
// characters the installer stores. The errors do not quote the seed.
func SigningKeyFromSeed(seed string) (*SigningKey, error) {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return nil, ErrNoSigningKey
	}
	raw, err := hex.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, fmt.Errorf("the bundle signing key is not %d bytes of hex", ed25519.SeedSize)
	}
	return &SigningKey{private: ed25519.NewKeyFromSeed(raw)}, nil
}

// SigningKeyPath is where the seed is in the vault, as the field
// signing_seed. Only the operator reads it, with its own vault identity:
// External Secrets is denied the path, so the key never becomes a Secret.
const SigningKeyPath = "gentian-os/kernel/backup/signing"

// SigningKeyFromVault reads the seed with read, the operator's vault client.
// A path that holds nothing (absent reports it) is a key that is absent,
// which is a state and not a fault: bundles are then written unsigned and
// say so.
func SigningKeyFromVault(ctx context.Context, read func(context.Context, string) (map[string]string, error), absent func(error) bool) (*SigningKey, error) {
	if read == nil {
		return nil, ErrNoSigningKey
	}
	data, err := read(ctx, SigningKeyPath)
	if err != nil {
		if absent != nil && absent(err) {
			return nil, ErrNoSigningKey
		}
		return nil, fmt.Errorf("the bundle signing key cannot be read from the vault: %w", err)
	}
	return SigningKeyFromSeed(data["signing_seed"])
}

// PublicKey is the public half, base64.
func (k *SigningKey) PublicKey() string {
	return base64.StdEncoding.EncodeToString(k.private.Public().(ed25519.PublicKey))
}

// ID names the key: the first sixteen hex characters of the SHA-256 of its
// public half.
func (k *SigningKey) ID() string {
	return keyID(k.private.Public().(ed25519.PublicKey))
}

func keyID(public []byte) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])[:16]
}

// SignManifest signs a manifest as manifest.json will hold it.
func (k *SigningKey) SignManifest(manifest []byte) *bundle.ManifestSignature {
	return &bundle.ManifestSignature{
		Algorithm: bundle.SignatureAlgorithm,
		PublicKey: k.PublicKey(),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(k.private, signedMessage(manifest))),
	}
}

// signedMessage is what a signature is over: the context, then the
// manifest without the final newline the file is written with.
func signedMessage(manifest []byte) []byte {
	return append([]byte(bundle.SignatureContext), bytes.TrimSuffix(manifest, []byte("\n"))...)
}

// The answers a provenance check gives, by name. A restore records the name
// and says the sentence.
const (
	ProvenanceProven       = "Proven"
	ProvenanceNoKey        = "NoSigningKey"
	ProvenanceUnsigned     = "Unsigned"
	ProvenanceOtherKey     = "SignedByAnotherKey"
	ProvenanceBadSignature = "BadSignature"
	ProvenanceNoClusterID  = "NoClusterID"
	ProvenanceOtherCluster = "AnotherCluster"
)

// Provenance is what a restore can tell of where a bundle comes from.
type Provenance struct {
	// Reason is one of the Provenance* names.
	Reason string
	// Message says it in a sentence, for a person.
	Message string
}

// Proven reports whether the bundle was signed by this cluster's key and
// taken on this cluster.
func (p Provenance) Proven() bool { return p.Reason == ProvenanceProven }

// CheckProvenance says whether a manifest was written on this cluster: signed
// by own, the cluster's own key, and naming ownUID, the cluster's own id.
// plain is the manifest as it was read, decrypted; m is what it parses to;
// sig is the bundle's signature, nil when it holds none.
//
// Only this cluster's key is checked with. The public key a signature
// names says which key signed; a key the bundle itself supplies would prove
// nothing.
func CheckProvenance(plain []byte, m *Manifest, sig *bundle.ManifestSignature, own *SigningKey, ownUID string) Provenance {
	switch {
	case own == nil:
		return Provenance{ProvenanceNoKey, "this cluster holds no bundle signing key (gentian-os/kernel/backup/signing), so it cannot tell whether it wrote the bundle"}
	case sig == nil:
		return Provenance{ProvenanceUnsigned, "the bundle is not signed: it was written before bundles were signed, or by a platform that does not sign them"}
	}
	public, perr := base64.StdEncoding.DecodeString(sig.PublicKey)
	signature, serr := base64.StdEncoding.DecodeString(sig.Signature)
	if sig.Algorithm != bundle.SignatureAlgorithm || perr != nil || serr != nil || len(public) != ed25519.PublicKeySize {
		return Provenance{ProvenanceBadSignature, "the bundle's signature is NOT one this platform can read: it was damaged or made up"}
	}
	mine := own.private.Public().(ed25519.PublicKey)
	if !bytes.Equal(public, mine) {
		return Provenance{ProvenanceOtherKey, fmt.Sprintf(
			"the bundle is signed with another key (%s) than this cluster's (%s): it was written on another cluster, or on this one before its bundle signing key was replaced",
			keyID(public), keyID(mine))}
	}
	if !ed25519.Verify(mine, signedMessage(plain), signature) {
		return Provenance{ProvenanceBadSignature, "the bundle's signature does NOT match its manifest: the manifest was changed after this cluster signed it, or the signature is another bundle's"}
	}
	switch {
	case ownUID == "":
		return Provenance{ProvenanceNoClusterID, "this cluster has no cluster id (spec.clusterUID of its Cluster claim), so it cannot tell whether the bundle was taken on it"}
	case m == nil || m.ClusterUID != ownUID:
		uid := ""
		if m != nil {
			uid = m.ClusterUID
		}
		if uid == "" {
			uid = "none"
		}
		return Provenance{ProvenanceOtherCluster, fmt.Sprintf(
			"the bundle is signed with this cluster's key and was taken on a cluster of another id (%s; this one is %s): one built from the same recovery kit and another deployments repository", uid, ownUID)}
	}
	return Provenance{ProvenanceProven, "the bundle is signed with this cluster's key and was taken on this cluster (" + ownUID + ")"}
}
