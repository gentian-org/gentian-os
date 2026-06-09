// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package stagingca

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBuildTrustStoreJKSFromLeaf(t *testing.T) {
	leaf := testLeafPEM(t)
	bundle, err := BuildBundle(leaf)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	jks, err := BuildTrustStoreJKS(bundle, TrustStorePassword)
	if err != nil {
		t.Fatalf("BuildTrustStoreJKS: %v", err)
	}
	if err := VerifyTrustStoreJKS(jks, TrustStorePassword); err != nil {
		t.Fatalf("VerifyTrustStoreJKS: %v", err)
	}
}

func TestBuildTrustStoreJKSKeytoolCompatible(t *testing.T) {
	if _, err := exec.LookPath("keytool"); err != nil {
		t.Skip("keytool not available")
	}
	leaf := testLeafPEM(t)
	bundle, err := BuildBundle(leaf)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	jks, err := BuildTrustStoreJKS(bundle, TrustStorePassword)
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
