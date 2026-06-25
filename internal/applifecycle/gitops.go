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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GitOps edits gentian-deployments tenant YAML and pushes commits.
type GitOps struct {
	path string
	repo string
}

// NewGitOps returns a GitOps helper. Path must be a local git checkout.
func NewGitOps(path, repo string) *GitOps {
	return &GitOps{path: path, repo: repo}
}

func (g *GitOps) enabled() bool {
	return g.path != ""
}

func (g *GitOps) ensureRepo() error {
	if g.path == "" {
		return fmt.Errorf("GENTIAN_DEPLOYMENTS_PATH is not configured")
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
	matches, _ := filepath.Glob(filepath.Join(g.path, "**", "tenants", tenant+".yaml"))
	for _, p := range matches {
		return p, nil
	}
	err := filepath.WalkDir(g.path, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(b)
		if strings.Contains(text, "kind: Tenant") && strings.Contains(text, "name: "+tenant) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("tenant file for %q not found in deployments repo", tenant)
	}
	return matches[0], nil
}

func (g *GitOps) Install(tenant, profile, actor string) (string, error) {
	if !g.enabled() {
		return "", fmt.Errorf("gitops backend requires deployments path")
	}
	file, err := g.tenantFile(tenant)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	text := string(content)
	profileLine := regexp.MustCompile(`(?m)^\s+profile:\s+` + regexp.QuoteMeta(profile) + `\s*$`)
	if profileLine.MatchString(text) {
		return "already_installed", nil
	}
	appsHeader := regexp.MustCompile(`(?m)^  apps:\s*$`)
	if appsHeader.MatchString(text) {
		text = appsHeader.ReplaceAllString(text, "  apps:\n  - profile: "+profile)
	} else {
		text = strings.TrimRight(text, "\n") + "\n  apps:\n  - profile: " + profile + "\n"
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		return "", err
	}
	if err := g.commit(file, fmt.Sprintf("feat(%s): install %s (via gtnctl by %s)", tenant, profile, actor)); err != nil {
		return "", err
	}
	return "installed", nil
}

func (g *GitOps) Uninstall(tenant, profile, actor string) (string, error) {
	if !g.enabled() {
		return "", fmt.Errorf("gitops backend requires deployments path")
	}
	file, err := g.tenantFile(tenant)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`(?m)^  - profile: ` + regexp.QuoteMeta(profile) + `\s*\n`)
	text := string(content)
	newText := re.ReplaceAllString(text, "")
	if newText == text {
		return "not_installed", nil
	}
	if err := os.WriteFile(file, []byte(newText), 0o644); err != nil {
		return "", err
	}
	if err := g.commit(file, fmt.Sprintf("feat(%s): uninstall %s (via gtnctl by %s)", tenant, profile, actor)); err != nil {
		return "", err
	}
	return "uninstalled", nil
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
	commit := exec.Command("git", "-C", g.path, "commit", "-m", message)
	if out, err := commit.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	push := exec.Command("git", "-C", g.path, "push")
	if out, err := push.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w: %s", err, out)
	}
	return nil
}
