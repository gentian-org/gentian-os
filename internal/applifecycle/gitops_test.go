/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing limitations under the License.
*/

package applifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitOps_tenantFile_prefersActiveTenantsPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cluster := "test"
	stage := "dev"
	tenant := "demo"

	defPath := filepath.Join(root, "clusters", "other", "definitions", tenant, "prod")
	if err := os.MkdirAll(defPath, 0o755); err != nil {
		t.Fatal(err)
	}
	defFile := filepath.Join(defPath, "tenant.yaml")
	if err := os.WriteFile(defFile, []byte("kind: Tenant\nmetadata:\n  name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	activePath := filepath.Join(root, "clusters", cluster, "tenants", tenant, stage)
	if err := os.MkdirAll(activePath, 0o755); err != nil {
		t.Fatal(err)
	}
	activeFile := filepath.Join(activePath, "tenant.yaml")
	if err := os.WriteFile(activeFile, []byte("kind: Tenant\nmetadata:\n  name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	g := NewGitOps(root, "", cluster, stage)
	got, err := g.tenantFile(tenant)
	if err != nil {
		t.Fatalf("tenantFile: %v", err)
	}
	if got != activeFile {
		t.Fatalf("tenantFile = %q, want %q", got, activeFile)
	}
}
