/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing limitations and the License.
*/

package applifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GitOps edits gentian-deployments tenant YAML and pushes commits.
type GitOps struct {
	path    string
	repo    string
	cluster string
	stage   string
}

// NewGitOps returns a GitOps helper. Path must be a local git checkout.
func NewGitOps(path, repo, cluster, stage string) *GitOps {
	return &GitOps{path: path, repo: repo, cluster: cluster, stage: stage}
}

func (g *GitOps) requirePath() error {
	if g.path == "" {
		return fmt.Errorf("GENTIAN_DEPLOYMENTS_PATH is not configured")
	}
	return nil
}

func (g *GitOps) ensureRepo() error {
	if err := g.requirePath(); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(g.path, ".git")); err == nil {
		return nil
	}
	if g.repo == "" {
		return fmt.Errorf("deployments git repository not configured")
	}
	cmd := exec.Command("git", "clone", g.repo, g.path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (g *GitOps) tenantFile(tenant string) (string, error) {
	if err := g.ensureRepo(); err != nil {
		return "", err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	stage := g.stage
	if stage == "" {
		stage = "dev"
	}

	// Active GitOps path synced by Argo CD (clusters/<cluster>/tenants/<tenant>/<stage>/).
	preferred := filepath.Join(g.path, "clusters", cluster, "tenants", tenant, stage, "tenant.yaml")
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
		return "", fmt.Errorf("tenant file for %q not found under clusters/%s/tenants (stage %q)", tenant, cluster, stage)
	}
	return found[0], nil
}

// Install adds profile to tenant YAML in git. Returns status, tenant file path, and whether git changed.
func (g *GitOps) Install(tenant, profile, actor string) (status, file string, changed bool, err error) {
	file, err = g.tenantFile(tenant)
	if err != nil {
		return "", "", false, err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", file, false, err
	}
	text := string(content)
	profileLine := regexp.MustCompile(`(?m)^\s+profile:\s+` + regexp.QuoteMeta(profile) + `\s*$`)
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
	if err := g.commit(file, fmt.Sprintf("feat(%s): install %s (via %s)", tenant, profile, actor)); err != nil {
		return "", file, false, err
	}
	return "installed", file, true, nil
}

// Uninstall removes profile from tenant YAML in git.
func (g *GitOps) Uninstall(tenant, profile, actor string) (status, file string, changed bool, err error) {
	file, err = g.tenantFile(tenant)
	if err != nil {
		return "", "", false, err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", file, false, err
	}
	re := regexp.MustCompile(`(?m)^  - profile: ` + regexp.QuoteMeta(profile) + `\s*\n`)
	text := string(content)
	newText := re.ReplaceAllString(text, "")
	if newText == text {
		return "not_installed", file, false, nil
	}
	if err := os.WriteFile(file, []byte(newText), 0o644); err != nil {
		return "", file, false, err
	}
	if err := g.commit(file, fmt.Sprintf("feat(%s): uninstall %s (via %s)", tenant, profile, actor)); err != nil {
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

func (g *GitOps) commit(file, message string) error {
	rel, err := filepath.Rel(g.path, file)
	if err != nil {
		rel = file
	}
	add := exec.Command("git", "-C", g.path, "add", rel)
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	diff := exec.Command("git", "-C", g.path, "diff", "--cached", "--quiet")
	if err := diff.Run(); err == nil {
		return nil
	}
	commit := exec.Command("git", "-C", g.path, "commit", "-m", message)
	if out, err := commit.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	pull := exec.Command("git", "-C", g.path, "pull", "--rebase", "--autostash")
	if out, err := pull.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull --rebase: %w: %s", err, out)
	}
	push := exec.Command("git", "-C", g.path, "push")
	if out, err := push.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w: %s", err, out)
	}
	return nil
}
