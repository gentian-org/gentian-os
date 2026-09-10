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

package applifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGitOps_tenantFile_prefersActiveTenantsPath(t *testing.T) {

	root := t.TempDir()
	cluster := "test"
	tenant := "demo"

	defPath := filepath.Join(root, "clusters", "other", "definitions", tenant)
	if err := os.MkdirAll(defPath, 0o755); err != nil {
		t.Fatal(err)
	}
	defFile := filepath.Join(defPath, "tenant.yaml")
	if err := os.WriteFile(defFile, []byte("kind: Tenant\nmetadata:\n  name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	activePath := filepath.Join(root, "clusters", cluster, "tenants", tenant)
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
	gitScript := filepath.Join(root, "git")
	if err := os.WriteFile(gitScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))

	g := NewGitOps(root, "", cluster)
	got, err := g.tenantFile(context.Background(), tenant)
	if err != nil {
		t.Fatalf("tenantFile: %v", err)
	}
	if got != activeFile {
		t.Fatalf("tenantFile = %q, want %q", got, activeFile)
	}
}
