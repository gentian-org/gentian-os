/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package trustanchor

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBuildTrustStoreJKSFromLeaf(t *testing.T) {
	leaf := testLeafPEM(t)
	bundle, err := BuildBundle(context.Background(), leaf)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	jks, err := BuildTrustStoreJKS(bundle.CACrt, TrustStorePassword)
	if err != nil {
		t.Fatalf("BuildTrustStoreJKS: %v", err)
	}
	if err := verifyTrustStoreJKS(jks, TrustStorePassword); err != nil {
		t.Fatalf("verifyTrustStoreJKS: %v", err)
	}
}

func TestBuildTrustStoreJKSKeytoolCompatible(t *testing.T) {
	if _, err := exec.LookPath("keytool"); err != nil {
		t.Skip("keytool not available")
	}
	leaf := testLeafPEM(t)
	bundle, err := BuildBundle(context.Background(), leaf)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	jks, err := BuildTrustStoreJKS(bundle.CACrt, TrustStorePassword)
	if err != nil {
		t.Fatalf("BuildTrustStoreJKS: %v", err)
	}

	dir := t.TempDir()
	jksPath := filepath.Join(dir, "truststore.jks")
	if err := os.WriteFile(jksPath, jks, 0o600); err != nil {
		t.Fatalf("write jks: %v", err)
	}
	cmd := exec.Command("keytool", "-list", "-keystore", jksPath, "-storetype", "JKS", "-storepass", TrustStorePassword)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("keytool -list: %v\n%s", err, out)
	}
	if len(out) == 0 {
		t.Fatal("expected keytool output")
	}
}

// verifyTrustStoreJKS checks that the JKS can be loaded and contains at least
// one trusted certificate.
func verifyTrustStoreJKS(jksData []byte, storePassword string) error {
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
