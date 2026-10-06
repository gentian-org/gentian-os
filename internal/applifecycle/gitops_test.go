/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
