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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitOps_Install_commitShape(t *testing.T) {
	root, remote := initGitOpsFixture(t)
	g := NewGitOps(root, remote, "demo-cluster")

	tenantYAML := `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  displayName: Demo
  apps:
  - profile: nextcloud
`
	tenantDir := filepath.Join(root, "clusters", "demo-cluster", "tenants", "demo")
	if err := os.MkdirAll(tenantDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tenantFile := filepath.Join(tenantDir, "tenant.yaml")
	if err := os.WriteFile(tenantFile, []byte(tenantYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, root, "init tenant")

	status, file, changed, err := g.Install(context.Background(), "demo", "element", "tester")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if status != "installed" || !changed {
		t.Fatalf("status=%q changed=%v", status, changed)
	}
	if file != tenantFile {
		t.Fatalf("file = %q", file)
	}

	logOut := gitLogLast(t, root)
	if !strings.Contains(logOut, "feat(demo): install element (via tester)") {
		t.Fatalf("commit message = %q", logOut)
	}
	content, err := os.ReadFile(tenantFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "profile: element") {
		t.Fatalf("tenant.yaml missing installed profile: %s", content)
	}
}

func TestGitOps_Uninstall_commitShape(t *testing.T) {
	root, remote := initGitOpsFixture(t)
	g := NewGitOps(root, remote, "demo-cluster")

	tenantYAML := `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  displayName: Demo
  apps:
  - profile: nextcloud
  - profile: element
`
	tenantDir := filepath.Join(root, "clusters", "demo-cluster", "tenants", "demo")
	if err := os.MkdirAll(tenantDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tenantFile := filepath.Join(tenantDir, "tenant.yaml")
	if err := os.WriteFile(tenantFile, []byte(tenantYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, root, "init tenant")

	status, _, changed, err := g.Uninstall(context.Background(), "demo", "element", "tester")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if status != "uninstalled" || !changed {
		t.Fatalf("status=%q changed=%v", status, changed)
	}
	logOut := gitLogLast(t, root)
	if !strings.Contains(logOut, "feat(demo): uninstall element (via tester)") {
		t.Fatalf("commit message = %q", logOut)
	}
}

func initGitOpsFixture(t *testing.T) (root, remote string) {
	t.Helper()
	root = t.TempDir()
	remote = filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--bare", remote)
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "test")
	runGit(t, root, "remote", "add", "origin", remote)
	return root, remote
}

func gitCommitAll(t *testing.T, root, message string) {
	t.Helper()
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", message)
	runGit(t, root, "push", "-u", "origin", "HEAD")
}

func gitLogLast(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "log", "-1", "--pretty=%s").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
