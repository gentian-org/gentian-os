/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// Renaming a catalogue in place.
//
// A catalogue's name is carried by three things at once: the source entry
// (spec.catalogue.sources[].name on the tenant's manifest or the Cluster
// claim), the catalogue: of every app entry and add-on pin installed from
// it, and the origin of every profile materialised from it -- on the
// bundle beside the profile (profilebundle.OriginAnnotation) and on each
// build under builds/. Removing the source and adding it under another name
// leaves the origins at the old name, so the next install from the new one
// is refused as another origin's (nameTaken), and the profile is never
// residue while an app is installed from it. So a rename rewrites all
// three, as one commit.
//
// Nothing else changes. The bytes the digest was taken over are the profile
// file, which carries no origin, so every pin stays at its digest and the
// operator rolls nothing: it reads the origin to decide whose a profile is
// and whether its bundle may hold companions, and both answers are the same
// under the new name.

// RenameCatalogueSource renames the catalogue old to new: one of tenant's
// own with a tenant, one of the whole cluster's with tenant "". The source
// entry, every app entry and add-on pin of the tenant -- of every tenant,
// for a catalogue of the cluster -- that names it, and the origin of every
// materialised profile, bundle and build that came from it are rewritten
// in one commit.
//
// Refused when old is not a catalogue of that scope (ErrCatalogueNotFound),
// when new is one already -- of the same scope, of the cluster for a
// tenant's, of any tenant for the cluster's -- (ErrCatalogueNameTaken), and
// when a name is not a name (ErrInvalidName). The same name again is
// "unchanged".
func (g *GitOps) RenameCatalogueSource(ctx context.Context, tenant, old, new string, meta Meta) (Result, error) {
	if !ValidName(old) {
		return Result{}, fmt.Errorf("%w: catalogue %q", ErrInvalidName, old)
	}
	if !ValidName(new) {
		return Result{}, fmt.Errorf("%w: a catalogue's name is lower-case letters, digits and hyphens", ErrInvalidName)
	}
	if tenant != "" && !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	scope := "cluster"
	if tenant != "" {
		scope = tenant
	}
	msg := fmt.Sprintf("feat(%s): rename catalogue %s to %s (via %s)", scope, old, new, meta.actor())
	g.mu.Lock()
	defer g.mu.Unlock()
	for started, attempt := time.Now(), 1; ; attempt++ {
		if err := g.ensureRepo(ctx); err != nil {
			return Result{}, err
		}
		rels, err := g.renameSource(ctx, tenant, old, new)
		if err != nil {
			return Result{}, err
		}
		if len(rels) == 0 {
			return Result{Status: "unchanged"}, nil
		}
		err = g.commitPaths(ctx, rels, msg, meta)
		if err == nil {
			return g.landed(ctx, "renamed")
		}
		if !errors.Is(err, errPushRejected) {
			return Result{}, err
		}
		if err := retryPush(ctx, started, attempt); err != nil {
			return Result{}, err
		}
	}
}

// renameSource writes the rename into the checkout and answers the
// repository-relative paths it changed; none when old is new. The caller
// holds the lock and has synced.
func (g *GitOps) renameSource(ctx context.Context, tenant, old, new string) ([]string, error) {
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	files := map[string]string{}
	var oldOrigin, newOrigin string
	if tenant == "" {
		claimPath := filepath.Join(g.path, "clusters", cluster, "kernel", "claims", "cluster.yaml")
		raw, err := os.ReadFile(claimPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoClusterClaim
		}
		if err != nil {
			return nil, err
		}
		sources, err := claimCatalogueSources(string(raw))
		if err != nil {
			return nil, err
		}
		at := -1
		for i, src := range sources {
			switch src.Name {
			case old:
				at = i
			case new:
				return nil, fmt.Errorf("%w: %s is already a catalogue of this cluster, at %s",
					ErrCatalogueNameTaken, new, src.where())
			}
		}
		if at < 0 {
			return nil, fmt.Errorf("%w: %s", ErrCatalogueNotFound, old)
		}
		if old == new {
			return nil, nil
		}
		// As adding one is refused: to a tenant that has the name, it would
		// mean two catalogues.
		manifests, err := g.manifestsOfTenants(cluster)
		if err != nil {
			return nil, err
		}
		var holders []string
		for _, m := range manifests {
			own, err := parseTenantCatalogue(m.text)
			if err != nil {
				return nil, err
			}
			for _, src := range own.Sources {
				if src.Name == new {
					holders = append(holders, m.name)
				}
			}
		}
		if len(holders) > 0 {
			sort.Strings(holders)
			return nil, fmt.Errorf("%w: %s is a catalogue of tenant %s; remove it there or choose another name",
				ErrCatalogueNameTaken, new, strings.Join(holders, ", "))
		}
		sources[at].Name = new
		out, err := setClaimSources(string(raw), sources)
		if err != nil {
			return nil, err
		}
		files[claimPath] = out
		for _, m := range manifests {
			next, n, err := renameAppCatalogues(m.text, old, new)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", m.name, err)
			}
			if n > 0 {
				files[m.path] = next
			}
		}
		oldOrigin, newOrigin = profilebundle.ClusterOrigin(old), profilebundle.ClusterOrigin(new)
	} else {
		file, err := g.tenantFileRead(ctx, tenant)
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		have, err := parseTenantCatalogue(string(raw))
		if err != nil {
			return nil, err
		}
		at := -1
		for i, src := range have.Sources {
			switch src.Name {
			case old:
				at = i
			case new:
				return nil, fmt.Errorf("%w: %s is already a catalogue of this tenant", ErrCatalogueNameTaken, new)
			}
		}
		if at < 0 {
			return nil, fmt.Errorf("%w: %s", ErrCatalogueNotFound, old)
		}
		if old == new {
			return nil, nil
		}
		claim, err := g.claimSourcesLocked(cluster)
		if err != nil {
			return nil, err
		}
		for _, src := range claim {
			if src.Name == new {
				return nil, fmt.Errorf("%w: %s is a catalogue of the whole cluster, which this tenant already sees; choose another name",
					ErrCatalogueNameTaken, new)
			}
		}
		have.Sources[at].Name = new
		out, err := setTenantCatalogue(string(raw), have)
		if err != nil {
			return nil, err
		}
		if out, _, err = renameAppCatalogues(out, old, new); err != nil {
			return nil, err
		}
		files[file] = out
		oldOrigin, newOrigin = profilebundle.TenantOrigin(tenant, old), profilebundle.TenantOrigin(tenant, new)
	}
	if err := renameOrigins(filepath.Join(g.path, "clusters", cluster, CatalogueDir), oldOrigin, newOrigin, files); err != nil {
		return nil, err
	}
	rels := make([]string, 0, len(files))
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(g.path, path)
		if err != nil {
			return nil, err
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels, nil
}

// manifestOfTenant is one tenant's manifest as the checkout holds it.
type manifestOfTenant struct {
	name, path, text string
}

// manifestsOfTenants reads every tenant's manifest under the cluster. The
// caller holds the lock and has synced. One that does not parse is an
// error: what it names is then not known.
func (g *GitOps) manifestsOfTenants(cluster string) ([]manifestOfTenant, error) {
	root := filepath.Join(g.path, "clusters", cluster, "tenants")
	var out []manifestOfTenant
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(path) != "tenant.yaml" {
			return walkErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var doc struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			rel, _ := filepath.Rel(g.path, path)
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		if !ValidName(doc.Metadata.Name) {
			return nil
		}
		out = append(out, manifestOfTenant{name: doc.Metadata.Name, path: path, text: string(raw)})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// claimSourcesLocked reads the claim's sources as they are written; none
// for a cluster without a claim. The caller holds the lock and has synced.
func (g *GitOps) claimSourcesLocked(cluster string) ([]CatalogueSource, error) {
	raw, err := os.ReadFile(filepath.Join(g.path, "clusters", cluster, "kernel", "claims", "cluster.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return claimCatalogueSources(string(raw))
}

// catalogueLine is the one scalar key of an app entry or an add-on pin
// that names a catalogue.
var catalogueLine = regexp.MustCompile(`^(\s+)catalogue:\s*([^\s#]+)(\s*(?:#.*)?)$`)

// renameAppCatalogues rewrites catalogue: old to new on every app entry and
// add-on pin of a tenant manifest's text, and answers how many it rewrote.
// Only those lines are touched -- the entry's own key, and the key of a pin
// under its addonPins; a value of the same name under the app's config is
// the chart's -- and the text is read back and held to what the rename
// means: the manifest with those values changed and nothing else. One that
// reads back as anything else is an error, and nothing is committed.
func renameAppCatalogues(text, old, new string) (string, int, error) {
	var before, after map[string]any
	if err := yaml.Unmarshal([]byte(text), &before); err != nil {
		return "", 0, fmt.Errorf("parse tenant manifest: %w", err)
	}
	spec, _ := before["spec"].(map[string]any)
	apps, _ := spec["apps"].([]any)
	count := 0
	var entries []string
	for _, raw := range apps {
		app, _ := raw.(map[string]any)
		was := count
		if app["catalogue"] == old {
			app["catalogue"] = new
			count++
		}
		pins, _ := app["addonPins"].([]any)
		for _, p := range pins {
			if pin, _ := p.(map[string]any); pin["catalogue"] == old {
				pin["catalogue"] = new
				count++
			}
		}
		if count > was {
			name, _ := app["profile"].(string)
			entries = append(entries, name)
		}
	}
	if count == 0 {
		return text, 0, nil
	}
	lines := strings.Split(text, "\n")
	for _, name := range entries {
		start, end, keyIndent, ok := appEntryExtent(lines, name)
		if !ok {
			return "", 0, fmt.Errorf("the entry of %s is not written as an app entry is", name)
		}
		pinsFrom, pinsTo := -1, -1
		if at := findKeyLine(lines, start+1, end, keyIndent, "addonPins"); at >= 0 {
			pinsFrom, pinsTo = at+1, listEnd(lines, at+1, keyIndent)
		}
		for i := start + 1; i < end; i++ {
			m := catalogueLine.FindStringSubmatch(lines[i])
			if m == nil || strings.Trim(m[2], `"'`) != old {
				continue
			}
			if indentOf(lines[i]) == keyIndent || (i >= pinsFrom && i < pinsTo) {
				lines[i] = m[1] + "catalogue: " + new + m[3]
			}
		}
	}
	out := strings.Join(lines, "\n")
	if err := yaml.Unmarshal([]byte(out), &after); err != nil {
		return "", 0, fmt.Errorf("the edited manifest is not valid YAML: %w", err)
	}
	if !reflect.DeepEqual(before, after) {
		return "", 0, errors.New("the edited manifest does not read back as written")
	}
	return out, count, nil
}

// listEnd is blockEnd for a key at indent whose value is a list, counting
// a list written at the key's own indentation -- as the director writes a
// pin, and a marshalled manifest writes every list -- as the key's.
func listEnd(lines []string, from, indent int) int {
	for i := from; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if in := indentOf(lines[i]); in < indent || (in == indent && !strings.HasPrefix(t, "- ") && t != "-") {
			return i
		}
	}
	return len(lines)
}

// renameOrigins puts, into files, every file of the catalogue directory
// whose origin is from, with the origin to: the bundle patch beside each
// profile, and each build under builds/. The bytes the digest was taken
// over are not among them. Each file is read back and held to the one
// change.
func renameOrigins(dir, from, to string, files map[string]string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	annotationLine := regexp.MustCompile(`^(\s+)` + regexp.QuoteMeta(profilebundle.OriginAnnotation) + `:\s*\S.*$`)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".bundle.yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		have, err := bundleAnnotations(raw)
		if err != nil {
			return fmt.Errorf("catalogue: the bundle %s does not parse: %w", entry.Name(), err)
		}
		if have[profilebundle.OriginAnnotation] != from {
			continue
		}
		out, err := rewriteOneLine(string(raw), annotationLine, func(indent string) string {
			return indent + profilebundle.OriginAnnotation + ": " + quoteScalar(to)
		})
		if err != nil {
			return fmt.Errorf("catalogue: the origin of %s: %w", entry.Name(), err)
		}
		back, err := bundleAnnotations([]byte(out))
		if err != nil || back[profilebundle.OriginAnnotation] != to || back[profilebundle.Annotation] != have[profilebundle.Annotation] {
			return fmt.Errorf("catalogue: the bundle %s does not read back as written", entry.Name())
		}
		files[path] = out
	}

	builds, err := os.ReadDir(filepath.Join(dir, BuildsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	originLine := regexp.MustCompile(`^(  )origin:\s*\S.*$`)
	for _, entry := range builds {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, BuildsDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		have, err := buildSpec(raw)
		if err != nil {
			return fmt.Errorf("catalogue: the build %s does not parse: %w", entry.Name(), err)
		}
		if have.Origin != from {
			continue
		}
		out, err := rewriteOneLine(string(raw), originLine, func(indent string) string {
			return indent + "origin: " + quoteScalar(to)
		})
		if err != nil {
			return fmt.Errorf("catalogue: the origin of build %s: %w", entry.Name(), err)
		}
		back, err := buildSpec([]byte(out))
		if err != nil || back.Origin != to || back.Profile != have.Profile || back.Digest != have.Digest || back.Bundle != have.Bundle {
			return fmt.Errorf("catalogue: the build %s does not read back as written", entry.Name())
		}
		files[path] = out
	}
	return nil
}

// bundleAnnotations reads the annotations a bundle patch puts on its profile.
func bundleAnnotations(raw []byte) (map[string]string, error) {
	var patch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		return nil, err
	}
	return patch.Metadata.Annotations, nil
}

// buildSpecFields is what a ProfileBuild states.
type buildSpecFields struct {
	Profile, Digest, Origin, Bundle string
}

// buildSpec reads a ProfileBuild's spec.
func buildSpec(raw []byte) (buildSpecFields, error) {
	var build struct {
		Spec buildSpecFields `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &build); err != nil {
		return buildSpecFields{}, err
	}
	return build.Spec, nil
}

// rewriteOneLine replaces the one line of text that matches line with what
// with makes of its indentation (the first group). Two such lines, or none,
// is an error: the file is not the one this editor knows.
func rewriteOneLine(text string, line *regexp.Regexp, with func(indent string) string) (string, error) {
	lines := strings.Split(text, "\n")
	at := -1
	for i, l := range lines {
		if !line.MatchString(l) {
			continue
		}
		if at >= 0 {
			return "", errors.New("it is stated twice")
		}
		at = i
	}
	if at < 0 {
		return "", errors.New("it is not written on a line of its own")
	}
	lines[at] = with(line.FindStringSubmatch(lines[at])[1])
	return strings.Join(lines, "\n"), nil
}
