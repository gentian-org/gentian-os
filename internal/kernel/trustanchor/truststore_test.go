/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trustanchor

import (
	"context"
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
	if err := VerifyTrustStoreJKS(jks, TrustStorePassword); err != nil {
		t.Fatalf("VerifyTrustStoreJKS: %v", err)
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
