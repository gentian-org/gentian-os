/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package trustanchor

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"time"
)

const (
	// TrustStoreKey is the secret data key for the Java truststore.
	TrustStoreKey = "truststore.jks"
	// TrustStorePassword is the default JKS password (staging dev clusters only).
	TrustStorePassword = "changeit"
)

// BuildTrustStoreJKS imports every PEM CERTIFICATE block from bundlePEM into a
// new Java KeyStore (JKS) suitable for javax.net.ssl.trustStore.
func BuildTrustStoreJKS(bundlePEM []byte, storePassword string) ([]byte, error) {
	certs, err := parsePEMCertificates(bundlePEM)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificates in bundle")
	}
	return encodeTrustedCertsJKS(certs, storePassword)
}

func parsePEMCertificates(pemData []byte) ([][]byte, error) {
	var certs [][]byte
	rest := pemData
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			continue
		}
		certs = append(certs, append([]byte(nil), block.Bytes...))
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no PEM CERTIFICATE blocks found")
	}
	return certs, nil
}

// encodeTrustedCertsJKS writes a minimal JKS with trusted certificate entries.
// Format: Java KeyStore version 2 (trusted cert tag 0x01).
func encodeTrustedCertsJKS(certs [][]byte, storePassword string) ([]byte, error) {
	var body bytes.Buffer
	if err := binary.Write(&body, binary.BigEndian, uint32(0xFEEDFEED)); err != nil {
		return nil, err
	}
	if err := binary.Write(&body, binary.BigEndian, uint32(2)); err != nil {
		return nil, err
	}
	if err := binary.Write(&body, binary.BigEndian, uint32(len(certs))); err != nil {
		return nil, err
	}

	now := time.Now()
	for i, der := range certs {
		if err := binary.Write(&body, binary.BigEndian, uint32(2)); err != nil { // trusted cert entry
			return nil, err
		}
		alias := fmt.Sprintf("trust-anchor-%d", i)
		if err := writeUTF(&body, alias); err != nil {
			return nil, err
		}
		if err := binary.Write(&body, binary.BigEndian, uint64(now.UnixMilli())); err != nil {
			return nil, err
		}
		if err := writeUTF(&body, "X.509"); err != nil {
			return nil, err
		}
		if err := binary.Write(&body, binary.BigEndian, uint32(len(der))); err != nil {
			return nil, err
		}
		if _, err := body.Write(der); err != nil {
			return nil, err
		}
	}

	digest := sha1.Sum(jksIntegrityInput(body.Bytes(), storePassword))
	if _, err := body.Write(digest[:]); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

func writeUTF(w *bytes.Buffer, s string) error {
	data := []byte(s)
	if len(data) > 0xFFFF {
		return fmt.Errorf("UTF string too long")
	}
	if err := binary.Write(w, binary.BigEndian, uint16(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// jksIntegrityInput matches sun.security.provider.JavaKeyStore#getPreKeyedHash
// followed by the encoded keystore body (without the trailing digest).
func jksIntegrityInput(body []byte, password string) []byte {
	var prefix []byte
	for _, r := range password {
		prefix = append(prefix, byte(r>>8), byte(r))
	}
	prefix = append(prefix, []byte("Mighty Aphrodite")...)
	out := make([]byte, 0, len(prefix)+len(body))
	out = append(out, prefix...)
	out = append(out, body...)
	return out
}
