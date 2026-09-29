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

package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Signing is AD-2's other half.
//
// Argo CD syncs the deployments repository only for commits signed by the
// director or by the break-glass key. This is the director's side: it holds a
// private key, and every commit it makes carries a signature made with it.
//
// Without a key the director commits unsigned, and says so once at start. That
// is deliberate rather than fatal. A cluster whose deployments checkout
// predates signing has no keys, its AppProject therefore carries no policy,
// and a director that refused to start would take the console down over a
// constraint nothing is enforcing. What it must never do is sign with
// something other than the key the repository names — so there is no fallback
// key and no self-generated one: either the key it was given, or nothing.

// EnableSigning imports a private key and makes every later commit use it.
//
// The key arrives as an armoured OpenPGP block, from the Secret External
// Secrets projects out of the vault. It is imported into a keyring of this
// process's own under dir, never the image's default: two directors on one
// node would otherwise share a keyring, and a keyring inside the container
// image would be baked into every cluster.
func (g *GitOps) EnableSigning(ctx context.Context, armouredKey, home string) error {
	if strings.TrimSpace(armouredKey) == "" {
		return fmt.Errorf("signing: the key is empty")
	}
	if home == "" {
		return fmt.Errorf("signing: no keyring directory")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("signing: keyring directory: %w", err)
	}
	// 0700 whatever the umask said: gpg refuses to use a keyring others can
	// read, and the refusal arrives as an unrelated-looking import failure.
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("signing: keyring directory mode: %w", err)
	}

	imp := exec.CommandContext(ctx, "gpg", "--homedir", home, "--batch", "--yes", "--quiet",
		"--pinentry-mode", "loopback", "--passphrase", "", "--import")
	imp.Stdin = strings.NewReader(armouredKey)
	if out, err := imp.CombinedOutput(); err != nil {
		return fmt.Errorf("signing: importing the key: %w: %s", err, out)
	}

	fpr, err := signingFingerprint(ctx, home)
	if err != nil {
		return err
	}

	// Ultimate owner-trust on the key this director was handed.
	//
	// Without it a signature verifies as "good, but I do not know whether to
	// trust this key" -- git prints U rather than G -- because owner-trust is
	// a statement about the holder and an imported key carries none. In a
	// keyring holding exactly this process's own identity there is nothing to
	// be undecided about: the key IS the director. Argo CD makes the same
	// judgement on its side by treating presence in its keyring as trust.
	trust := exec.CommandContext(ctx, "gpg", "--homedir", home, "--batch", "--yes", "--quiet",
		"--import-ownertrust")
	trust.Stdin = strings.NewReader(fpr + ":6:\n")
	if out, err := trust.CombinedOutput(); err != nil {
		return fmt.Errorf("signing: trusting the key: %w: %s", err, out)
	}

	// A wrapper, because git's gpg.program takes a program and not a command
	// line, and gpg needs both the homedir and the loopback answer on every
	// call it makes.
	wrapper := filepath.Join(home, "git-gpg")
	script := fmt.Sprintf("#!/bin/sh\nexec gpg --homedir %q --batch --yes "+
		"--pinentry-mode loopback --passphrase '' \"$@\"\n", home)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		return fmt.Errorf("signing: wrapper: %w", err)
	}

	g.signArgs = []string{
		"-c", "gpg.program=" + wrapper,
		"-c", "gpg.format=openpgp",
		"-c", "user.signingkey=" + fpr,
		"-c", "commit.gpgsign=true",
	}
	g.signingKey = fpr
	return nil
}

// SigningKey is the fingerprint commits are signed with, or empty.
func (g *GitOps) SigningKey() string { return g.signingKey }

// signingFingerprint is the one secret key in this keyring.
//
// Exactly one: a keyring with two would leave which key signed up to gpg's
// idea of a default, and the cluster trusts specific key ids. Two is a
// mistake in what was projected into the Secret, and it is worth failing on
// rather than resolving by picking.
func signingFingerprint(ctx context.Context, home string) (string, error) {
	list := exec.CommandContext(ctx, "gpg", "--homedir", home, "--batch",
		"--list-secret-keys", "--with-colons")
	out, err := list.Output()
	if err != nil {
		return "", fmt.Errorf("signing: reading the keyring: %w", err)
	}
	var fprs []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 9 && f[0] == "fpr" && f[9] != "" {
			fprs = append(fprs, f[9])
		}
	}
	switch len(fprs) {
	case 0:
		return "", fmt.Errorf("signing: the key imported and the keyring holds no secret key")
	case 1:
		return fprs[0], nil
	default:
		return "", fmt.Errorf("signing: the keyring holds %d secret keys; "+
			"the cluster trusts specific ids, so which one signs cannot be left to a default", len(fprs))
	}
}
