// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package stagingca

import (
	"bytes"
	"crypto/sha1"
	"crypto/x509"
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
		alias := fmt.Sprintf("staging-ca-%d", i)
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

// VerifyTrustStoreJKS checks that the JKS can be loaded and contains at least
// one trusted certificate. Used by tests only.
func VerifyTrustStoreJKS(jksData []byte, storePassword string) error {
	// Parse header only — full verification would require java keytool.
	if len(jksData) < 12 {
		return fmt.Errorf("JKS too short")
	}
	if binary.BigEndian.Uint32(jksData[0:4]) != 0xFEEDFEED {
		return fmt.Errorf("invalid JKS magic")
	}
	if binary.BigEndian.Uint32(jksData[4:8]) != 2 {
		return fmt.Errorf("unsupported JKS version")
	}
	count := int(binary.BigEndian.Uint32(jksData[8:12]))
	if count < 1 {
		return fmt.Errorf("empty keystore")
	}
	// Ensure at least one cert parses from the bundle path used to build it.
	_, err := x509.ParseCertificate(certsFromJKSFirstEntry(jksData))
	return err
}

func certsFromJKSFirstEntry(jksData []byte) []byte {
	// Skip magic(4) + version(4) + count(4) + tag(1) + alias UTF
	off := 12
	if off+4 > len(jksData) || binary.BigEndian.Uint32(jksData[off:off+4]) != 2 {
		return nil
	}
	off += 4
	if off+2 > len(jksData) {
		return nil
	}
	aliasLen := int(binary.BigEndian.Uint16(jksData[off : off+2]))
	off += 2 + aliasLen + 8 // timestamp uint64
	if off+2 > len(jksData) {
		return nil
	}
	certTypeLen := int(binary.BigEndian.Uint16(jksData[off : off+2]))
	off += 2 + certTypeLen
	if off+4 > len(jksData) {
		return nil
	}
	certLen := int(binary.BigEndian.Uint32(jksData[off : off+4]))
	off += 4
	if off+certLen > len(jksData) {
		return nil
	}
	return jksData[off : off+certLen]
}
