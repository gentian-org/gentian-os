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
	"path/filepath"
	"strings"
)

// A profile in the cluster, because a tenant asked for it (AD-3).
//
// The cluster holds no catalogue. A ComponentProfile is written here when a
// tenant installs the entry it describes, at the digest the store named, and
// it stays because the tenant still has it installed.
//
// Committed rather than applied. The director writes git and the operator
// reads the cluster, and a profile that arrived any other way would be the one
// object in the system whose presence nothing explains — no commit, no author,
// and no way to tell a profile somebody installed from one somebody put there.

// CatalogueDir is where materialised profiles live in the deployments
// repository, beside the cluster they were materialised for.
const CatalogueDir = "catalogue"

// MaterialiseProfile writes one profile into the cluster's catalogue
// directory, if it is not already there byte for byte.
//
// body is what the source served and what the digest was taken over. It is
// written unchanged: re-serialising it would commit bytes nobody verified,
// and the digest in the commit message would then describe something else.
func (g *GitOps) MaterialiseProfile(ctx context.Context, name, digest string, body []byte, meta Meta) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, name)
	}
	if len(body) == 0 {
		return Result{}, fmt.Errorf("catalogue: %s has no body", name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	dir := filepath.Join(g.path, "clusters", g.cluster, CatalogueDir)
	path := filepath.Join(dir, name+".yaml")
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(body) {
		// The same entry at the same digest. Installing what another tenant
		// already installed is the common case on a cluster with more than
		// one tenant, and it is not a change.
		return Result{Status: "unchanged"}, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return Result{}, err
	}

	// The kustomization Argo syncs this directory through.
	kustomization := filepath.Join(dir, "kustomization.yaml")
	listed, err := os.ReadFile(kustomization)
	if err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}
	next, changed := ensureListed(string(listed), name+".yaml", listResource)
	files := []string{path}
	if changed {
		if err := os.WriteFile(kustomization, []byte(next), 0o644); err != nil {
			return Result{}, err
		}
		files = append(files, kustomization)
	}

	rels := make([]string, 0, len(files))
	for _, f := range files {
		rel, err := filepath.Rel(g.path, f)
		if err != nil {
			return Result{}, err
		}
		rels = append(rels, rel)
	}
	// The digest is in the message because it is the thing that was checked,
	// and a reviewer asking "is this the entry the store meant" should not
	// have to hash the file to find out.
	msg := fmt.Sprintf("feat(catalogue): materialise %s at %s", name, shortDigest(digest))
	if err := g.commitPaths(ctx, rels, msg, meta); err != nil {
		return Result{}, err
	}
	head, err := g.head(ctx)
	if err != nil {
		return Result{Status: "materialised"}, nil
	}
	return Result{Status: "materialised", Changed: true, Commit: head}, nil
}

// shortDigest is the first twelve hex characters, which is enough to tell two
// versions of an entry apart in a log without filling the line.
func shortDigest(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "sha256:" + d
}
