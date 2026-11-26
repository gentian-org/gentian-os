/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package vouchingkey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"

	"github.com/go-jose/go-jose/v4"
)

// A generated key reads back, signs a statement its own public half
// verifies, and names itself the same way every time.
func TestGeneratedKeySignsWhatItsPublicHalfVerifies(t *testing.T) {
	pemBytes, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	key, err := Parse(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(pemBytes)
	if err != nil || again.ID() != key.ID() || key.ID() == "" {
		t.Fatalf("the id is not stable: %q then %q (%v)", key.ID(), again.ID(), err)
	}
	signer, err := key.Signer()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign([]byte(`{"sub":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := key.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 1 || set.Keys[0].KeyID != key.ID() || set.Keys[0].Algorithm != "ES256" || set.Keys[0].Use != "sig" || !set.Keys[0].IsPublic() {
		t.Fatalf("the set is not this key's public half: %s", raw)
	}
	parsed, err := jose.ParseSignedCompact(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Signatures[0].Header.KeyID != key.ID() {
		t.Fatalf("the statement names %q, not the key", parsed.Signatures[0].Header.KeyID)
	}
	if _, err := parsed.Verify(set.Keys[0]); err != nil {
		t.Fatalf("the public half does not verify the statement: %v", err)
	}
}

// Only what Generate writes is read: no other encoding, curve or kind.
func TestParseRefusesWhatThePlatformDidNotIssue(t *testing.T) {
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384DER, _ := x509.MarshalPKCS8PrivateKey(p384)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sec1, _ := x509.MarshalECPrivateKey(p256)
	for name, in := range map[string][]byte{
		"nothing":       nil,
		"not PEM":       []byte("secret"),
		"another curve": pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: p384DER}),
		"an RSA key":    pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: rsaDER}),
		"SEC 1":         pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}),
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s was read as an issued key", name)
		}
	}
}
