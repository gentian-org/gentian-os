/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// A profile in the cluster, because a tenant asked for it (AD-3).
//
// The cluster holds no catalogue. A ComponentProfile is written here when a
// tenant installs the entry it describes, at the digest the install asked
// for, and it stays because the tenant still has it installed.
//
// With it goes its bundle: the bytes the digest was taken over, kept so that
// the operator can check at rollout that the profile it holds is that build.
//
// Committed rather than applied. The director writes git and the operator
// reads the cluster, and a profile that arrived any other way would be the one
// object in the system whose presence nothing explains — no commit, no author,
// and no way to tell a profile somebody installed from one somebody put there.

// CatalogueDir is where materialised profiles live in the deployments
// repository, beside the cluster they were materialised for.
const CatalogueDir = "catalogue"

// BundleFile is the file a profile's bundle is committed in, beside the
// profile: the verified bytes again, as the annotation the operator checks a
// pinned install against.
func BundleFile(name string) string { return name + ".bundle.yaml" }

// ErrBundleTooLarge is a profile whose bytes cannot be carried beside it for
// the operator to check, and which is therefore not materialised at all: an
// install pinned to a digest nothing can verify at rollout would be held
// there for good.
var ErrBundleTooLarge = errors.New("catalogue: the profile is too large to carry its bundle")

// MaterialiseProfile writes one profile into the cluster's catalogue
// directory, with its bundle, if both are not already there byte for byte.
//
// body is what the source served and what the digest was taken over. It is
// written unchanged: re-serialising it would commit bytes nobody verified,
// and the digest in the commit message would then describe something else.
//
// The same bytes are written a second time, as a kustomize patch that puts
// them on the profile in an annotation. The profile that reaches the cluster
// is not these bytes -- the API server prunes and defaults what Argo CD
// applies -- so the operator could not otherwise recompute the digest an
// install is pinned to. With the bundle it can: it hashes the annotation's
// bytes and checks the profile against what they say
// (internal/profilebundle). The patch is derived from body and from nothing
// else, and the operator does not take its word for anything.
func (g *GitOps) MaterialiseProfile(ctx context.Context, name, digest string, body []byte, meta Meta) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: profile %q", ErrInvalidName, name)
	}
	if len(body) == 0 {
		return Result{}, fmt.Errorf("catalogue: %s has no body", name)
	}
	if len(body) > profilebundle.MaxBytes {
		return Result{}, fmt.Errorf("%w: %s is %d bytes and the limit is %d",
			ErrBundleTooLarge, name, len(body), profilebundle.MaxBytes)
	}
	bundle, err := renderBundle(name, body)
	if err != nil {
		return Result{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	dir := filepath.Join(g.path, "clusters", g.cluster, CatalogueDir)
	path := filepath.Join(dir, name+".yaml")
	bundlePath := filepath.Join(dir, BundleFile(name))
	kustomization := filepath.Join(dir, "kustomization.yaml")
	listed, err := os.ReadFile(kustomization)
	if err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}
	// The kustomization Argo syncs this directory through: the profile as a
	// resource, its bundle as a patch on it.
	next, _ := ensureListed(string(listed), name+".yaml", listResource)
	next, _ = ensureListed(next, BundleFile(name), listPatch)

	same := func(file string, want []byte) bool {
		have, err := os.ReadFile(file)
		return err == nil && bytes.Equal(have, want)
	}
	if same(path, body) && same(bundlePath, bundle) && next == string(listed) {
		// The same entry at the same digest. Installing what another tenant
		// already installed is the common case on a cluster with more than
		// one tenant, and it is not a change.
		return Result{Status: "unchanged"}, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	files := []string{}
	for _, f := range []struct {
		path string
		want []byte
	}{{path, body}, {bundlePath, bundle}, {kustomization, []byte(next)}} {
		if same(f.path, f.want) {
			continue
		}
		if err := os.WriteFile(f.path, f.want, 0o644); err != nil {
			return Result{}, err
		}
		files = append(files, f.path)
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
	// and a reviewer asking "is this the build that was asked for" should
	// not have to hash the file to find out.
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

// renderBundle is the patch that puts a profile's verified bytes on it.
//
// It names its target by the apiVersion the profile itself states -- and by
// its namespace, should the document state one although the kind has none --
// because a patch that names anything else matches nothing, and kustomize
// then fails the whole directory rather than the one entry.
func renderBundle(name string, body []byte) ([]byte, error) {
	var head struct {
		APIVersion string `json:"apiVersion"`
		Metadata   struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("catalogue: %s does not parse: %w", name, err)
	}
	if head.APIVersion == "" {
		return nil, fmt.Errorf("catalogue: %s states no apiVersion", name)
	}
	var b strings.Builder
	b.WriteString("# The bytes of " + name + ".yaml as the catalogue source served them, for the\n")
	b.WriteString("# operator to check a pinned install against. Written by the director with\n")
	b.WriteString("# the profile; do not edit either without the other.\n")
	b.WriteString("apiVersion: " + quoteScalar(head.APIVersion) + "\n")
	b.WriteString("kind: ComponentProfile\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + name + "\n")
	if head.Metadata.Namespace != "" {
		b.WriteString("  namespace: " + quoteScalar(head.Metadata.Namespace) + "\n")
	}
	b.WriteString("  annotations:\n")
	b.WriteString("    " + profilebundle.Annotation + ": \"" + profilebundle.Encode(body) + "\"\n")
	return []byte(b.String()), nil
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
