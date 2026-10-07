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
	"encoding/json"
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
// It arrives as a bundle: one file, the profile first and after it whatever
// travels with it (profilebundle/bundle.go). The file is committed whole, so
// Argo CD applies the profile and its companions together.
//
// With it goes its carrier: the bytes the digest was taken over, kept so that
// the operator can check at rollout that the profile and each companion it
// holds are that build.
//
// Nothing is taken away by itself. The Application that syncs this directory
// does not prune (kernel/appsets/raw/11b-catalogue.yaml), so a companion a
// newer build no longer brings stays in the cluster, like a profile whose file
// is gone, until somebody removes it.
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

// ErrProfileStatesOrigin is a served profile that carries an annotation only
// the platform writes: its bundle, or where it came from.
var ErrProfileStatesOrigin = errors.New("catalogue: the profile states what only the platform records")

// ErrProfileNameTaken is a profile that cannot be materialised under its name
// because a profile from somewhere else already has it. ComponentProfiles are
// cluster-scoped: one name, one profile, whoever published it.
type ErrProfileNameTaken struct {
	// Name is the profile's name.
	Name string
	// Origin is where the profile being materialised comes from.
	Origin string
	// Holder is the origin of the profile that has the name; empty for one
	// with no recorded origin.
	Holder string
	// Platform says the name is one of the profiles the platform ships.
	Platform bool
}

func (e *ErrProfileNameTaken) Error() string {
	return fmt.Sprintf("catalogue: the name %s is taken on this cluster by a profile of another origin", e.Name)
}

// platformProfiles are the ComponentProfiles the platform's own chart ships
// (charts/gentian-os/templates/componentprofile-*.yaml). They are not in the
// deployments repository, so nothing read from it shows the name as taken;
// a catalogue entry of the same name would have Argo CD and Helm each
// applying their own profile over the other's. A test holds this list to the
// chart.
var platformProfiles = map[string]bool{"admin-console": true, "concierge": true, "desktop": true}

// PlatformProfile reports whether name is a profile the platform ships.
func PlatformProfile(name string) bool { return platformProfiles[name] }

// MaterialisedProfile is what the deployments repository says about one
// profile of the cluster's catalogue directory.
type MaterialisedProfile struct {
	// Present says the directory holds a profile of this name: one the
	// director materialised or the installer scaffolded.
	Present bool
	// Origin is the catalogue it was materialised from, as its bundle
	// records it (profilebundle.OriginAnnotation); empty when none is
	// recorded.
	Origin string
}

// ProfileOnCluster answers whether the cluster's catalogue directory holds a
// profile, and where it came from.
//
// "On the cluster" is answered from git, like everything else this service
// knows: the directory is what Argo CD applies, and a profile reaches the
// cluster in no other way but the platform's own chart.
func (g *GitOps) ProfileOnCluster(ctx context.Context, name string) (MaterialisedProfile, error) {
	if !ValidName(name) {
		return MaterialisedProfile{}, fmt.Errorf("%w: profile %q", ErrInvalidName, name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return MaterialisedProfile{}, err
	}
	return g.materialised(name)
}

// materialised reads one profile's presence and origin from the checkout.
// The caller holds the lock and has synced.
func (g *GitOps) materialised(name string) (MaterialisedProfile, error) {
	dir := filepath.Join(g.path, "clusters", g.cluster, CatalogueDir)
	if _, err := os.Stat(filepath.Join(dir, name+".yaml")); err != nil {
		if os.IsNotExist(err) {
			return MaterialisedProfile{}, nil
		}
		return MaterialisedProfile{}, err
	}
	out := MaterialisedProfile{Present: true}
	raw, err := os.ReadFile(filepath.Join(dir, BundleFile(name)))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return MaterialisedProfile{}, err
	}
	var patch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		return MaterialisedProfile{}, fmt.Errorf("catalogue: the bundle of %s does not parse: %w", name, err)
	}
	out.Origin = patch.Metadata.Annotations[profilebundle.OriginAnnotation]
	return out, nil
}

// nameTaken applies the rule for a name that is already a profile's.
//
// The same origin again is the same catalogue publishing its own entry: a
// reinstall, or a new build. Otherwise a tenant's catalogue never takes a
// name somebody else has, and nobody takes a name a tenant's catalogue has --
// in both directions, because the profile is one object for the whole
// cluster and a tenant's is installable only in that tenant. Between two
// catalogues of the whole cluster the later build replaces the earlier, as it
// always has: an install pinned to the earlier one is then held by the
// operator, which says so.
func nameTaken(name, origin string, have MaterialisedProfile) error {
	if PlatformProfile(name) {
		return &ErrProfileNameTaken{Name: name, Origin: origin, Platform: true}
	}
	if !have.Present || have.Origin == origin {
		return nil
	}
	incoming, err := profilebundle.ParseOrigin(origin)
	if err != nil {
		return fmt.Errorf("catalogue: origin %q: %w", origin, err)
	}
	holder, err := profilebundle.ParseOrigin(have.Origin)
	if err != nil || incoming.Tenant != "" || holder.Tenant != "" {
		return &ErrProfileNameTaken{Name: name, Origin: origin, Holder: have.Origin}
	}
	return nil
}

// MaterialiseProfile writes one profile's bundle into the cluster's catalogue
// directory, with its carrier, if both are not already there byte for byte.
//
// body is what the source served and what the digest was taken over: the
// profile and its companions, one file, listed in the kustomization once so
// that all of it is applied. It is
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
//
// origin is the catalogue the bytes were fetched from
// (profilebundle.ClusterOrigin or TenantOrigin), written on the profile by
// the same patch. A name another origin holds is refused with
// ErrProfileNameTaken (see nameTaken), and nothing is written.
func (g *GitOps) MaterialiseProfile(ctx context.Context, name, digest string, body []byte, origin string, meta Meta) (Result, error) {
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
	if parsed, err := profilebundle.ParseOrigin(origin); err != nil || parsed.Source == "" {
		return Result{}, fmt.Errorf("%w: origin %q", ErrInvalidName, origin)
	}
	bundle, err := renderBundle(name, body, origin)
	if err != nil {
		return Result{}, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Against the remote's own state, not a checkout that may be a few
	// seconds behind: whether the name is free is the thing being decided.
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	have, err := g.materialised(name)
	if err != nil {
		return Result{}, err
	}
	if err := nameTaken(name, origin, have); err != nil {
		return Result{}, err
	}

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
//
// What the fetch checked is checked here again, where it is written: nothing
// is committed that is not a bundle this origin may bring, whoever calls.
func renderBundle(name string, body []byte, origin string) ([]byte, error) {
	read, err := profilebundle.Check(body, name, origin)
	if err != nil {
		return nil, fmt.Errorf("catalogue: %s: %w", name, err)
	}
	var head struct {
		APIVersion string `json:"apiVersion"`
		Metadata   struct {
			Namespace   string            `json:"namespace"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	// The profile is the bundle's first document; what follows it is not
	// patched and carries nothing of the platform's.
	raw, err := json.Marshal(read.Profile)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("catalogue: %s does not parse: %w", name, err)
	}
	if head.APIVersion == "" {
		return nil, fmt.Errorf("catalogue: %s states no apiVersion", name)
	}
	// Whose a profile is, is recorded by the director from where it fetched
	// it. A profile that arrives saying so itself is a catalogue claiming to
	// be another one.
	for _, key := range []string{profilebundle.OriginAnnotation, profilebundle.Annotation} {
		if _, stated := head.Metadata.Annotations[key]; stated {
			return nil, fmt.Errorf("%w: %s states the annotation %s, which only the platform writes",
				ErrProfileStatesOrigin, name, key)
		}
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
	// Where it was fetched from, and so which tenants may install it.
	b.WriteString("    " + profilebundle.OriginAnnotation + ": " + quoteScalar(origin) + "\n")
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
