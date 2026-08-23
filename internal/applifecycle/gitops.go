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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// GitOps edits gentian-deployments tenant YAML and pushes commits.
type GitOps struct {
	path    string
	repo    string
	cluster string
}

// NewGitOps returns a GitOps helper. Path must be a local git checkout.
func NewGitOps(path, repo, cluster string) *GitOps {
	return &GitOps{path: path, repo: repo, cluster: cluster}
}

func (g *GitOps) requirePath() error {
	if g.path == "" {
		return fmt.Errorf("GENTIAN_DEPLOYMENTS_PATH is not configured")
	}
	return nil
}

// gitTimeout bounds every git invocation below.
//
// clone, pull and push all talk to a remote, and git has no default timeout for
// one that accepts the connection and then goes quiet. These run under an HTTP
// handler, so without a bound a hung remote holds the handler goroutine and the
// git process for as long as the process lives — and the caller hanging up does
// not release either.
const gitTimeout = 5 * time.Minute

// gitCmd builds a git invocation bounded by both the caller's context and
// gitTimeout, whichever ends first.
func (g *GitOps) gitCmd(ctx context.Context, args ...string) (*exec.Cmd, context.CancelFunc) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	return exec.CommandContext(cctx, "git", args...), cancel
}

func (g *GitOps) ensureRepo(ctx context.Context) error {
	if err := g.requirePath(); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(g.path, ".git")); err == nil {
		cmd, cancel := g.gitCmd(ctx, "-C", g.path, "pull", "--rebase", "--autostash")
		defer cancel()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git pull: %w", err)
		}
		return nil
	}
	if g.repo == "" {
		return fmt.Errorf("deployments git repository not configured")
	}
	cmd, cancel := g.gitCmd(ctx, "clone", g.repo, g.path)
	defer cancel()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (g *GitOps) tenantFile(ctx context.Context, tenant string) (string, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return "", err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}

	// Active GitOps path synced by Argo CD (clusters/<cluster>/tenants/<tenant>/).
	// No stage segment: a cluster has exactly one stage for its whole lifetime
	// (docs/deployment.md §1), so encoding it again under this cluster's own
	// tenants/ tree would be redundant.
	preferred := filepath.Join(g.path, "clusters", cluster, "tenants", tenant, "tenant.yaml")
	if _, err := os.Stat(preferred); err == nil {
		return preferred, nil
	}

	// Fallback: search only this cluster's tenants tree (never definitions/ templates).
	tenantsRoot := filepath.Join(g.path, "clusters", cluster, "tenants")
	var found []string
	err := filepath.WalkDir(tenantsRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		if filepath.Base(path) != "tenant.yaml" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(b)
		if strings.Contains(text, "kind: Tenant") && strings.Contains(text, "name: "+tenant) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "", fmt.Errorf("tenant file for %q not found under clusters/%s/tenants", tenant, cluster)
	}
	return found[0], nil
}

// Install adds profile to tenant YAML in git. Returns status, tenant file path, and whether git changed.
func (g *GitOps) Install(ctx context.Context, tenant, profile, actor string) (status, file string, changed bool, err error) {
	file, err = g.tenantFile(ctx, tenant)
	if err != nil {
		return "", "", false, err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", file, false, err
	}
	text := string(content)
	profileLine := regexp.MustCompile(`(?m)^\s*-\s*profile:\s*` + regexp.QuoteMeta(profile) + `\s*$`)
	if profileLine.MatchString(text) {
		return "already_installed", file, false, nil
	}
	text, ok := insertAppProfile(text, profile)
	if !ok {
		return "", file, false, fmt.Errorf("failed to update apps list in %s", file)
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		return "", file, false, err
	}
	if err := g.commit(ctx, file, fmt.Sprintf("feat(%s): install %s (via %s)", tenant, profile, actor)); err != nil {
		return "", file, false, err
	}
	return "installed", file, true, nil
}

// Uninstall removes profile from tenant YAML in git.
func (g *GitOps) Uninstall(ctx context.Context, tenant, profile, actor string) (status, file string, changed bool, err error) {
	file, err = g.tenantFile(ctx, tenant)
	if err != nil {
		return "", "", false, err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", file, false, err
	}
	// Remove the whole list entry, not just its `- profile:` line: an entry can carry
	// nested keys (addons, config) and leaving those behind produces YAML that does
	// not parse, which wedges every later reconcile. See removeAppEntry.
	text := string(content)
	newText, ok := removeAppEntry(text, profile)
	if !ok || newText == text {
		return "not_installed", file, false, nil
	}
	if err := os.WriteFile(file, []byte(newText), 0o644); err != nil {
		return "", file, false, err
	}
	if err := g.commit(ctx, file, fmt.Sprintf("feat(%s): uninstall %s (via %s)", tenant, profile, actor)); err != nil {
		return "", file, false, err
	}
	return "uninstalled", file, true, nil
}

func insertAppProfile(text, profile string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line == "  apps:" {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, "  - profile: "+profile)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n"), true
		}
	}
	return strings.TrimRight(text, "\n") + "\n  apps:\n  - profile: " + profile + "\n", true
}

func (g *GitOps) commit(ctx context.Context, file, message string) error {
	rel, err := filepath.Rel(g.path, file)
	if err != nil {
		rel = file
	}
	return g.commitPaths(ctx, []string{rel}, message)
}

// commitPaths stages repository-relative paths and pushes them as one commit.
func (g *GitOps) commitPaths(ctx context.Context, rels []string, message string) error {
	add, addCancel := g.gitCmd(ctx, append([]string{"-C", g.path, "add"}, rels...)...)
	defer addCancel()
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	diff, diffCancel := g.gitCmd(ctx, "-C", g.path, "diff", "--cached", "--quiet")
	defer diffCancel()
	if err := diff.Run(); err == nil {
		return nil
	}
	commit, commitCancel := g.gitCmd(ctx, "-C", g.path, "commit", "-m", message)
	defer commitCancel()
	if out, err := commit.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	pull, pullCancel := g.gitCmd(ctx, "-C", g.path, "pull", "--rebase", "--autostash")
	defer pullCancel()
	if out, err := pull.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull --rebase: %w: %s", err, out)
	}
	push, pushCancel := g.gitCmd(ctx, "-C", g.path, "push")
	defer pushCancel()
	if out, err := push.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w: %s", err, out)
	}
	return nil
}
