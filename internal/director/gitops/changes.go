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
	"path/filepath"
	"strconv"
	"strings"
)

// What changed, who changed it, and what allowed them to.
//
// This is the audit evidence the platform already had and never showed. Every
// change to a tenant's declared state is a commit; the director authors it as
// the person whose token authorised it and trailers it with the relation and
// object that permitted the change, joined to the request id. So "who changed
// this tenant's backup policy in March, and under what authority" is a git
// question, answered without storing anything twice.
//
// What it is NOT is the whole audit story. A sign-in leaves no commit, a
// refusal changes nothing so it commits nothing, and reading a secret writes
// nothing anywhere. Those need their own records, and the screen says so
// rather than implying this is everything.

// Change is one commit against a tenant's directory.
type Change struct {
	Commit string `json:"commit"`
	// Author is who made the change: the person the director acted for, or
	// whoever pushed by hand.
	Author Person `json:"author"`
	// At is when, RFC 3339, from the author date -- the moment the change was
	// made rather than the moment it was rewritten.
	At string `json:"at"`
	// Summary is the commit subject.
	Summary string `json:"summary"`
	// Files are the paths this commit touched, within the directory asked for.
	Files []string `json:"files"`

	// ThroughPlatform reports whether this change carries the director's
	// authorization trailer. False means it was pushed by hand, with whatever
	// credential the pusher held and no record of what allowed it -- which is
	// exactly what an audit wants to see marked rather than hidden.
	ThroughPlatform bool `json:"throughPlatform"`
	// Principal is the subject the trailer names: the stable identifier the
	// graph and the decision log use, which outlives a name or an address.
	Principal string `json:"principal,omitempty"`
	// Decision is what was asked of what -- "can_set_policy tenant:demo".
	Decision string `json:"decision,omitempty"`
	// RequestID joins this commit to the decision log entry and to the
	// gateway request that started it.
	RequestID string `json:"requestId,omitempty"`
}

// changeRecordSeparator and changeFieldSeparator are ASCII record and unit
// separators: they cannot occur in a commit subject, an address or a path, so
// the parse needs no quoting and cannot be confused by a subject containing
// whatever delimiter somebody chose.
const (
	changeRecordSeparator = "\x1e"
	changeFieldSeparator  = "\x1f"
)

// maxChanges bounds one answer. A tenant's whole history is not what a screen
// opens with, and an auditor asking for a year asks with a date.
const maxChanges = 500

// TenantChanges lists the commits that touched one tenant's directory.
func (g *GitOps) TenantChanges(ctx context.Context, tenant string, limit int, since string) ([]Change, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Rel(g.path, filepath.Dir(manifest))
	if err != nil {
		return nil, err
	}
	return g.changes(ctx, dir, limit, since)
}

// ClusterChanges lists the commits that touched this cluster's directory:
// every tenant, and the claims that describe the cluster itself.
func (g *GitOps) ClusterChanges(ctx context.Context, limit int, since string) ([]Change, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return nil, err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	return g.changes(ctx, filepath.Join("clusters", cluster), limit, since)
}

func (g *GitOps) changes(ctx context.Context, dir string, limit int, since string) ([]Change, error) {
	if limit <= 0 || limit > maxChanges {
		limit = maxChanges
	}
	// The record separator leads. Trailing it would put each commit's file
	// names, which --name-only writes after the format, at the head of the
	// NEXT record -- so every commit but the first would be parsed as a list
	// of paths and dropped.
	format := changeRecordSeparator + strings.Join([]string{
		"%H", "%an", "%ae", "%aI", "%s", "%(trailers:key=Gentian-Authz,valueonly)",
	}, changeFieldSeparator)

	args := []string{"-C", g.path, "log", "--format=format:" + format, "--name-only",
		"-n", strconv.Itoa(limit)}
	if since != "" {
		// Passed to git as-is: it accepts RFC 3339 and its own relative
		// spellings, and a value it cannot read is an error from git rather
		// than a window silently wider than asked for.
		args = append(args, "--since="+since)
	}
	args = append(args, "--", dir)

	cmd, cancel := g.gitCmd(ctx, args...)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	return parseChanges(string(out)), nil
}

// parseChanges turns git's output into records.
func parseChanges(out string) []Change {
	changes := []Change{}
	for _, record := range strings.Split(out, changeRecordSeparator) {
		if strings.TrimSpace(record) == "" {
			continue
		}
		// Five separators, six fields; the last carries the trailer and then
		// the file names, which git writes after the format.
		fields := strings.SplitN(record, changeFieldSeparator, 6)
		if len(fields) < 6 {
			continue
		}
		trailer, files, _ := strings.Cut(fields[5], "\n")
		change := Change{
			Commit:  fields[0],
			Author:  Person{Name: fields[1], Email: fields[2]},
			At:      fields[3],
			Summary: fields[4],
			Files:   []string{},
		}
		for _, f := range strings.Split(files, "\n") {
			if f = strings.TrimSpace(f); f != "" {
				change.Files = append(change.Files, f)
			}
		}
		applyAuthzTrailer(&change, trailer)
		changes = append(changes, change)
	}
	return changes
}

// applyAuthzTrailer reads what the director recorded, if anything.
//
// The trailer reads "req=<id> user:<subject> <relation> <object> allowed". A
// commit without one was not made through the platform, and that is reported
// rather than guessed at: the alternative would be a screen that showed a
// hand-pushed change as though something had authorised it.
func applyAuthzTrailer(change *Change, trailer string) {
	trailer = strings.TrimSpace(trailer)
	if trailer == "" {
		return
	}
	change.ThroughPlatform = true
	rest := trailer
	if id, after, found := strings.Cut(rest, " "); found && strings.HasPrefix(id, "req=") {
		change.RequestID = strings.TrimPrefix(id, "req=")
		rest = after
	}
	principal, after, found := strings.Cut(rest, " ")
	if !found {
		return
	}
	change.Principal = strings.TrimPrefix(principal, "user:")
	change.Decision = strings.TrimSuffix(strings.TrimSpace(after), " allowed")
}
