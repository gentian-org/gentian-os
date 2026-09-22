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

// Package gitops is the director's git backend: the only code in the platform
// that pushes to gentian-deployments (AD-2).
//
// It began as a copy of internal/applifecycle's GitOps and differs in what a
// commit says and how a push is allowed to fail. A commit is authored by the
// human whose token authorised it and carries the authorization decision as a
// trailer, so the repository is an audit log and not just a history. A rejected
// push is the concurrency signal: another writer got there first, so the change
// is rebased and retried instead of being serialised by a lock that only one
// process could hold.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// GitOps edits gentian-deployments tenant YAML and pushes commits.
type GitOps struct {
	path    string
	repo    string
	cluster string
	// committer is the director's own identity: the human is the author, the
	// director is what wrote the commit.
	committer Person
	// mu serialises operations on the one working tree this process owns.
	// It is not the concurrency control between writers — a rejected push is —
	// it only keeps two handlers from editing the same checkout at once.
	mu sync.Mutex
}

// Result is the outcome of a write.
type Result struct {
	// Status names what happened: installed, already_installed, uninstalled,
	// not_installed, updated, unchanged.
	Status string
	// Changed reports whether a commit was pushed.
	Changed bool
	// Commit is the pushed commit when Changed; the handle a caller is given.
	Commit string
}

// landed reads the commit just pushed. It runs under the same lock as the
// write, so it is this change's commit and not a neighbour's.
func (g *GitOps) landed(ctx context.Context, status string) (Result, error) {
	head, err := g.head(ctx)
	if err != nil {
		return Result{}, err
	}
	return Result{Status: status, Changed: true, Commit: head}, nil
}

// Person is a git identity.
type Person struct {
	Name  string
	Email string
}

// Meta is what a commit records beyond the change itself.
type Meta struct {
	// Author is the human whose token authorised the change.
	Author Person
	// Subject is the token's sub, the stable identifier the decision log and
	// the OpenFGA tuples use. Names and addresses change; this does not.
	Subject string
	// RequestID joins this commit to the issuer event and the decision log
	// entry (security principle 7).
	RequestID string
	// Decision is the authorization that allowed the change, as
	// "<relation> <object>" — what was asked, of what.
	Decision string
	// Principal replaces "user:<Subject>" in the trailer when what authorised
	// the change was not a person — a fact signed by the store, say.
	Principal string
}

func (m Meta) actor() string {
	if m.Principal != "" {
		return m.Principal
	}
	if m.Author.Email != "" {
		return identClean(m.Author.Email)
	}
	return m.Subject
}

// trailers renders the audit trailer: who was allowed what, under which
// request id. One line, so `git log --grep` answers "who did this and why was
// it allowed" without joining anything.
func (m Meta) trailers() []string {
	if m.Subject == "" && m.Decision == "" && m.Principal == "" {
		return nil
	}
	parts := []string{}
	if m.RequestID != "" {
		parts = append(parts, "req="+m.RequestID)
	}
	if m.Principal != "" {
		parts = append(parts, m.Principal)
	} else if m.Subject != "" {
		parts = append(parts, "user:"+m.Subject)
	}
	if m.Decision != "" {
		parts = append(parts, m.Decision, "allowed")
	}
	return []string{"Gentian-Authz: " + strings.Join(parts, " ")}
}

// ErrPushContended is returned when every retry lost the race to another writer.
var ErrPushContended = errors.New("push rejected after retries: another writer keeps winning")

// maxPushAttempts bounds the sync-edit-push loop.
const maxPushAttempts = 8

// NewGitOps returns a GitOps helper. Path must be a local git checkout.
func NewGitOps(path, repo, cluster string, committer Person) *GitOps {
	if committer.Name == "" {
		committer.Name = "gentian-director"
	}
	if committer.Email == "" {
		committer.Email = "director@gentian.invalid"
	}
	return &GitOps{path: path, repo: repo, cluster: cluster, committer: committer}
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

// ensureRepo makes the checkout equal to the remote. The checkout is never a
// source of truth — it is scratch space for composing one commit — so it is
// reset, not merged: anything in it that the remote does not have is the
// residue of a request that failed, and nobody was told it succeeded.
func (g *GitOps) ensureRepo(ctx context.Context) error {
	if err := g.requirePath(); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(g.path, ".git")); err != nil {
		if g.repo == "" {
			return fmt.Errorf("deployments git repository not configured")
		}
		cmd, cancel := g.gitCmd(ctx, "clone", g.repo, g.path)
		defer cancel()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone: %w: %s", err, out)
		}
		return nil
	}
	for _, args := range [][]string{
		{"rebase", "--abort"}, // best effort: only a crashed predecessor leaves one open
		{"fetch", "--prune", "origin"},
		{"reset", "--hard", "@{u}"},
		{"clean", "-fdq"},
	} {
		cmd, cancel := g.gitCmd(ctx, append([]string{"-C", g.path}, args...)...)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil && args[0] != "rebase" {
			return fmt.Errorf("git %s: %w: %s", args[0], err, out)
		}
	}
	return nil
}

// ErrTenantNotFound is returned when the repository has no manifest for a tenant.
var ErrTenantNotFound = errors.New("tenant not found")

// ErrInvalidName is returned for a tenant or profile name that is not a DNS label.
var ErrInvalidName = errors.New("invalid name")

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidName reports whether s is a DNS label. Names become path segments and
// are spliced into YAML, so nothing else is accepted, here or at the API.
func ValidName(s string) bool { return dnsLabel.MatchString(s) }

func (g *GitOps) tenantFile(ctx context.Context, tenant string) (string, error) {
	if !ValidName(tenant) {
		return "", fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if len(found) == 0 {
		return "", fmt.Errorf("%w: no manifest for %q under clusters/%s/tenants", ErrTenantNotFound, tenant, cluster)
	}
	return found[0], nil
}

// edit is a change to a tenant manifest as a function of its current text. It
// returns the new text, a status, and whether anything changed. Being a pure
// function is what makes a write retryable: when another writer wins the push,
// the edit is applied again to what they pushed, instead of asking git to
// merge two textual insertions at the same line — which it cannot.
type edit func(text string) (newText, status string, changed bool, err error)

// apply runs one edit as one commit: sync to the remote, edit, commit, push.
// A rejected push means the remote moved; the loop starts over from the new
// state. That is the whole of the concurrency control, and it is correct across
// replicas and across processes, which a mutex never was. It also keeps the
// answer honest: if the other writer made the same change, the retry reports
// that the state already holds rather than committing it twice.
func (g *GitOps) apply(ctx context.Context, tenant, message string, meta Meta, fn edit) (Result, error) {
	return g.applyTo(ctx, tenant, "", message, meta, fn)
}

// applyTo is apply for a file beside the tenant's manifest. sibling is a bare
// file name, "" for the manifest itself; a sibling that does not exist yet is
// edited from empty text.
func (g *GitOps) applyTo(ctx context.Context, tenant, sibling, message string, meta Meta, fn edit) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for attempt := 1; attempt <= maxPushAttempts; attempt++ {
		file, err := g.tenantFile(ctx, tenant)
		if err != nil {
			return Result{}, err
		}
		if sibling != "" {
			file = filepath.Join(filepath.Dir(file), sibling)
		}
		content, err := os.ReadFile(file)
		if err != nil && (sibling == "" || !errors.Is(err, os.ErrNotExist)) {
			return Result{}, err
		}
		text, status, changed, err := fn(string(content))
		if err != nil {
			return Result{}, fmt.Errorf("%w in %s", err, file)
		}
		if !changed {
			return Result{Status: status}, nil
		}
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			return Result{}, err
		}
		err = g.commit(ctx, file, message, meta)
		if err == nil {
			return g.landed(ctx, status)
		}
		if !errors.Is(err, errPushRejected) {
			return Result{}, err
		}
		// Back off by a random amount so writers that collided once do not
		// collide again in step.
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Duration(rand.Int63n(int64(attempt) * int64(40*time.Millisecond)))):
		}
	}
	return Result{}, ErrPushContended
}

// Install adds profile to the tenant's manifest.
func (g *GitOps) Install(ctx context.Context, tenant, profile string, meta Meta) (Result, error) {
	if !ValidName(profile) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, profile)
	}
	profileLine := regexp.MustCompile(`(?m)^\s*-\s*profile:\s*` + regexp.QuoteMeta(profile) + `\s*$`)
	msg := fmt.Sprintf("feat(%s): install %s (via %s)", tenant, profile, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		if profileLine.MatchString(text) {
			return text, "already_installed", false, nil
		}
		out, ok := insertAppProfile(text, profile)
		if !ok {
			return "", "", false, errors.New("failed to update apps list")
		}
		return out, "installed", true, nil
	})
}

// Uninstall removes profile from the tenant's manifest.
func (g *GitOps) Uninstall(ctx context.Context, tenant, profile string, meta Meta) (Result, error) {
	if !ValidName(profile) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, profile)
	}
	msg := fmt.Sprintf("feat(%s): uninstall %s (via %s)", tenant, profile, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		// Remove the whole list entry, not just its `- profile:` line: an entry can carry
		// nested keys (addons, config) and leaving those behind produces YAML that does
		// not parse, which wedges every later reconcile. See removeAppEntry.
		out, ok := removeAppEntry(text, profile)
		if !ok || out == text {
			return text, "not_installed", false, nil
		}
		return out, "uninstalled", true, nil
	})
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

func (g *GitOps) commit(ctx context.Context, file, message string, meta Meta) error {
	rel, err := filepath.Rel(g.path, file)
	if err != nil {
		rel = file
	}
	return g.commitPaths(ctx, []string{rel}, message, meta)
}

// commitPaths stages repository-relative paths and pushes them as one commit,
// authored by the human in meta and committed by the director.
//
// If the push does not land, the local commit is discarded, so a failed
// request leaves the checkout exactly where the remote is. A push refused
// because the remote moved is reported as errPushRejected for apply to retry.
func (g *GitOps) commitPaths(ctx context.Context, rels []string, message string, meta Meta) (err error) {
	defer func() {
		if err != nil {
			g.discardLocal(ctx)
		}
	}()
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

	full := message
	if t := meta.trailers(); len(t) > 0 {
		full += "\n\n" + strings.Join(t, "\n")
	}
	args := []string{"-C", g.path,
		"-c", "user.name=" + g.committer.Name,
		"-c", "user.email=" + g.committer.Email,
		"commit", "-m", full}
	if meta.Author.Name != "" || meta.Author.Email != "" {
		name, email := meta.Author.Name, meta.Author.Email
		if name == "" {
			name = meta.Subject
		}
		if email == "" {
			email = meta.Subject + "@users.gentian.invalid"
		}
		args = append(args, "--author", fmt.Sprintf("%s <%s>", identClean(name), identClean(email)))
	}
	commit, commitCancel := g.gitCmd(ctx, args...)
	defer commitCancel()
	if out, err := commit.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w: %s", err, out)
	}

	push, pushCancel := g.gitCmd(ctx, "-C", g.path, "push")
	defer pushCancel()
	if out, perr := push.CombinedOutput(); perr != nil {
		if pushRejected(string(out)) {
			return errPushRejected
		}
		return fmt.Errorf("git push: %w: %s", perr, out)
	}
	return nil
}

// errPushRejected is the optimistic-concurrency signal: another writer landed
// between this writer's sync and its push.
var errPushRejected = errors.New("push rejected: remote moved")

// identClean removes what git's ident syntax reserves. Name and address come
// from token claims a user can edit in their own profile; they must not be
// able to forge a second identity or a trailer.
func identClean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == '\n' || r == '\r' || r == 0 {
			return -1
		}
		return r
	}, s)
	if s = strings.TrimSpace(s); s == "" {
		return "unknown"
	}
	return s
}

// HasCommit reports whether id names a commit in the checkout.
func (g *GitOps) HasCommit(ctx context.Context, id string) bool {
	if !commitID.MatchString(id) {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cmd, cancel := g.gitCmd(ctx, "-C", g.path, "cat-file", "-e", id+"^{commit}")
	defer cancel()
	return cmd.Run() == nil
}

var commitID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// pushRejected reports whether a failed push lost a race rather than hit a
// real error. Only a race is worth retrying; a hook that refuses the push
// ("[remote rejected]") will refuse it again.
func pushRejected(out string) bool {
	return strings.Contains(out, "non-fast-forward") ||
		strings.Contains(out, "fetch first") ||
		strings.Contains(out, "cannot lock ref") ||
		strings.Contains(out, "failed to update ref")
}

// discardLocal drops commits that never reached the remote.
func (g *GitOps) discardLocal(ctx context.Context) {
	abort, c1 := g.gitCmd(ctx, "-C", g.path, "rebase", "--abort")
	_ = abort.Run()
	c1()
	reset, c2 := g.gitCmd(ctx, "-C", g.path, "reset", "--hard", "@{u}")
	_ = reset.Run()
	c2()
}

func (g *GitOps) head(ctx context.Context) (string, error) {
	cmd, cancel := g.gitCmd(ctx, "-C", g.path, "rev-parse", "HEAD")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// TenantFile returns the path of a tenant's manifest after syncing the
// checkout, for callers that read state rather than change it.
func (g *GitOps) TenantFile(ctx context.Context, tenant string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tenantFile(ctx, tenant)
}
