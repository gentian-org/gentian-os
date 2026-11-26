/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package vouchingkey is the key a component signs its statements about
// people with (requires.services.vouching.keys).
//
// The platform issues it: the operator makes one per component and tenant,
// puts the private half in the component's Secret and gives the public half
// to the tenant's realm. The component reads it and signs; it makes no key
// and publishes none, and the realm fetches nothing.
//
// One key type, so that every component and the realm agree without saying:
// ECDSA on P-256, statements signed ES256, the private half as PKCS #8 in
// PEM, and the key named by its RFC 7638 thumbprint.
package vouchingkey

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/go-jose/go-jose/v4"
)

const pemType = "PRIVATE KEY"

// Generate makes a new key and returns its private half in PEM.
func Generate() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der}), nil
}

// Key is an issued key, read from its PEM.
type Key struct {
	private *ecdsa.PrivateKey
	id      string
}

// Parse reads a key from the PEM Generate wrote. Anything else -- another
// encoding, another curve, another kind of key -- is refused, so a value
// that is not the platform's is never signed with.
func Parse(pemBytes []byte) (*Key, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != pemType {
		return nil, errors.New("not a PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not a PKCS #8 key: %w", err)
	}
	private, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || private.Curve != elliptic.P256() {
		return nil, errors.New("not an ECDSA key on P-256")
	}
	thumb, err := (&jose.JSONWebKey{Key: &private.PublicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, err
	}
	return &Key{private: private, id: base64.RawURLEncoding.EncodeToString(thumb)}, nil
}

// ID names the key: what a statement carries as kid.
func (k *Key) ID() string { return k.id }

// JWKS is the public half as the realm takes it: a set of this one key.
func (k *Key) JWKS() ([]byte, error) {
	return json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &k.private.PublicKey, KeyID: k.id, Algorithm: string(jose.ES256), Use: "sig",
	}}})
}

// Signer signs statements with the key, each naming it by its id.
func (k *Key) Signer() (jose.Signer, error) {
	return jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: k.private},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", k.id),
	)
}
