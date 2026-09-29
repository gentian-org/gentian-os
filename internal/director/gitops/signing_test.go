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

package gitops_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// gpgKey makes a throwaway signing key and returns it armoured, with its
// fingerprint. Skips where gpg is absent: the director's image has it, and a
// developer machine without it should not fail the suite.
func gpgKey(t *testing.T) (armoured, fingerprint string) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("needs gpg")
	}
	home := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		full := append([]string{"--homedir", home, "--batch", "--yes", "--quiet",
			"--pinentry-mode", "loopback", "--passphrase", ""}, args...)
		out, err := exec.Command("gpg", full...).Output()
		if err != nil {
			t.Fatalf("gpg %v: %v", args, err)
		}
		return string(out)
	}
	run("--quick-generate-key", "Test Director <d@example.test>", "ed25519", "sign", "never")
	for _, line := range strings.Split(run("--list-secret-keys", "--with-colons"), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 9 && f[0] == "fpr" && f[9] != "" {
			fingerprint = f[9]
			break
		}
	}
	return run("--armor", "--export-secret-keys"), fingerprint
}

// AD-2: what the director writes is signed, and the signature verifies
// against the key the cluster was told to trust. Without this the AppProject
// policy would refuse every write the director makes, and the first time
// anyone found out would be the first tenant.
func TestWhatTheDirectorCommitsIsSigned(t *testing.T) {
	key, fpr := gpgKey(t)
	remote := dt.Remote(t, "demo")
	path := dt.Clone(t, remote)
	g := gitops.NewGitOps(path, remote, dt.Cluster, director)

	home := filepath.Join(t.TempDir(), "keyring")
	if err := g.EnableSigning(context.Background(), key, home); err != nil {
		t.Fatalf("EnableSigning: %v", err)
	}
	if g.SigningKey() != fpr {
		t.Fatalf("signing key = %q, want %q", g.SigningKey(), fpr)
	}

	if _, err := g.Install(context.Background(), "demo", "element", meta("u-ada")); err != nil {
		t.Fatal(err)
	}

	// Verified with the same keyring, which is what Argo CD's repo-server
	// does with the keys the installer put in its ConfigMap.
	out, err := exec.Command("git", "-C", path,
		"-c", "gpg.program="+filepath.Join(home, "git-gpg"),
		"log", "-1", "--format=%G? %GK").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	got := strings.Fields(strings.TrimSpace(string(out)))
	if len(got) != 2 || got[0] != "G" {
		t.Fatalf("signature = %q, want a good signature", strings.TrimSpace(string(out)))
	}
	// %GK is the long key id: the last 16 of the fingerprint, and the form
	// AppProject.spec.sourceIntegrity lists.
	if !strings.HasSuffix(fpr, got[1]) {
		t.Fatalf("signed by %s, which is not the key that was imported (%s)", got[1], fpr)
	}
}

// A director with no key commits, and says nothing about signing. A cluster
// that predates signing has no keys and no policy; refusing to write would
// take the console down over a constraint nothing is enforcing.
func TestWithoutAKeyTheDirectorStillWrites(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if g.SigningKey() != "" {
		t.Fatalf("signing key = %q with no key given", g.SigningKey())
	}
	if _, err := g.Install(context.Background(), "demo", "element", meta("u-ada")); err != nil {
		t.Fatal(err)
	}
}

// Two secret keys in one keyring leaves which one signs to gpg's default,
// and the cluster trusts specific ids.
func TestTwoKeysAreRefusedRatherThanPickedBetween(t *testing.T) {
	first, _ := gpgKey(t)
	second, _ := gpgKey(t)
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	home := filepath.Join(t.TempDir(), "keyring")
	err := g.EnableSigning(context.Background(), first+"\n"+second, home)
	if err == nil || !strings.Contains(err.Error(), "cannot be left to a default") {
		t.Fatalf("two keys = %v, want a refusal", err)
	}
}

// An empty key is a misconfigured Secret, not a director that should sign
// with something else.
func TestAnEmptyKeyIsRefused(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if err := g.EnableSigning(context.Background(), "  \n", t.TempDir()); err == nil {
		t.Fatal("an empty key was accepted")
	}
}
