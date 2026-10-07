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
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Where a cluster's profiles may come from (AD-14).
//
// On the Cluster CLAIM, not in the director's environment. The difference is
// not cosmetic: a source is where software enters this cluster, and naming
// one is the platform administrator's act. On the claim it is a commit
// somebody reviewed, with an author and a date, next to everything else the
// cluster is. In an environment variable it is a deployment setting
// that changed when somebody rolled the Deployment, and the only record is
// whatever the pod spec happens to say now.
//
// It also makes the answer to "where could this cluster install software
// from" readable without cluster access, which is the question an audit asks
// and the one an environment variable answers worst.

// CatalogueSource is one repository of profile bundles.
//
// A source the claim names is offered to every tenant of the cluster. It is
// what the cluster offers, not a licence: an install is asked of the person
// (can_install_app), and whether the app then arrives is decided where its
// artefacts are pulled.
type CatalogueSource struct {
	// Name is the catalogue's slug: the first half of a coordinate, so
	// "main/nextcloud-base-ce" is served by the source named main.
	Name string `json:"name"`
	// URL is where bundles are fetched from.
	URL string `json:"url"`
}

// CatalogueSettings is the claim's whole spec.catalogue.
type CatalogueSettings struct {
	// StoreURL is where people are sent to get the entries this cluster
	// does not list itself -- everything maintained or licensed by somebody.
	//
	// Here rather than compiled in, because which store a cluster belongs to
	// is a fact about that cluster, and because a cluster with no store at
	// all is a supported thing: it says nothing and its own catalogues are
	// all there is.
	StoreURL string `json:"storeUrl,omitempty"`
	// Sources are the catalogues this cluster may fetch from.
	Sources []CatalogueSource `json:"sources,omitempty"`
}

// Catalogue reads spec.catalogue from the Cluster claim.
func (g *GitOps) Catalogue(ctx context.Context) (CatalogueSettings, error) {
	var claim struct {
		Spec struct {
			Catalogue CatalogueSettings `json:"catalogue"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return CatalogueSettings{}, err
	}
	out := CatalogueSettings{StoreURL: strings.TrimSpace(claim.Spec.Catalogue.StoreURL)}
	if !strings.HasPrefix(out.StoreURL, "https://") {
		out.StoreURL = ""
	}
	for _, src := range claim.Spec.Catalogue.Sources {
		src.Name = strings.TrimSpace(src.Name)
		src.URL = strings.TrimSpace(src.URL)
		if src.Name == "" || !strings.HasPrefix(src.URL, "https://") {
			// http:// is skipped here as it is everywhere else: the digest
			// makes the bytes safe, but a cluster fetching its catalogue in
			// clear announces what it runs.
			continue
		}
		out.Sources = append(out.Sources, src)
	}
	return out, nil
}

// CatalogueSources reads the sources this cluster declares.
//
// A source with no name or no https URL is skipped rather than refused: the
// claim is read on every start, and one malformed entry must not stop a
// director from serving. What it costs is that installs from that catalogue
// are refused for want of a source, which says the same thing where somebody
// will see it.
func (g *GitOps) CatalogueSources(ctx context.Context) ([]CatalogueSource, error) {
	c, err := g.Catalogue(ctx)
	if err != nil {
		return nil, err
	}
	return c.Sources, nil
}

// A tenant's own catalogues, and who may add them.
//
// On the tenant's manifest, for the reason the cluster's are on the claim: a
// catalogue is where software enters, and adding one is a commit with an
// author. Three things are recorded -- the sources only this tenant sees, who
// added each, and whether the tenant's own administrators may add any at all
// -- and the last two are the cluster administrator's alone to set. They are
// written by the methods below and by nothing else: no other write to a
// tenant's manifest touches spec.catalogue, and which of these methods a
// caller reaches is decided by the route it was authorised on.

// TenantCatalogueSource is one catalogue only a tenant sees.
type TenantCatalogueSource struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// AddedBy is "cluster" or "tenant". Anything else in the manifest is
	// read as "cluster": what nobody is known to have added, the tenant's
	// administrators may not remove.
	AddedBy string `json:"addedBy"`
}

// TenantCatalogue is a tenant's spec.catalogue.
type TenantCatalogue struct {
	// Delegated says the tenant's own administrators may add and remove
	// catalogues for this tenant.
	Delegated bool                    `json:"delegated"`
	Sources   []TenantCatalogueSource `json:"sources"`
}

// Errors of declaring a catalogue.
var (
	// ErrCatalogueNameTaken is a name that already means another catalogue
	// to the tenants that would see this one.
	ErrCatalogueNameTaken = errors.New("the catalogue name is taken")
	// ErrCatalogueNotFound is a catalogue that is not there to remove, or
	// not the caller's to remove.
	ErrCatalogueNotFound = errors.New("no such catalogue")
	// ErrCatalogueNotDelegated is a tenant whose own administrators may not
	// add catalogues.
	ErrCatalogueNotDelegated = errors.New("adding catalogues is not delegated to this tenant")
	// ErrCatalogueNotTenants is a catalogue the cluster's administrator
	// added, which the tenant's may not remove.
	ErrCatalogueNotTenants = errors.New("the catalogue was added by the cluster's administrator")
	// ErrCatalogueFlowForm is a catalogue section written in a form the
	// line editor cannot change.
	ErrCatalogueFlowForm = errors.New("the catalogue section is written on one line; write it as a block to change it here")
)

// parseTenantCatalogue reads spec.catalogue from a tenant manifest's text,
// keeping only what could be fetched from.
func parseTenantCatalogue(text string) (TenantCatalogue, error) {
	var doc struct {
		Spec struct {
			Catalogue *TenantCatalogue `json:"catalogue"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return TenantCatalogue{}, fmt.Errorf("parse tenant manifest: %w", err)
	}
	out := TenantCatalogue{}
	if doc.Spec.Catalogue == nil {
		return out, nil
	}
	out.Delegated = doc.Spec.Catalogue.Delegated
	seen := map[string]bool{}
	for _, src := range doc.Spec.Catalogue.Sources {
		src.Name, src.URL = strings.TrimSpace(src.Name), strings.TrimSpace(src.URL)
		if !ValidName(src.Name) || !strings.HasPrefix(src.URL, "https://") || seen[src.Name] {
			continue
		}
		seen[src.Name] = true
		if src.AddedBy != gentianov1alpha1.CatalogueAddedByTenant {
			src.AddedBy = gentianov1alpha1.CatalogueAddedByCluster
		}
		out.Sources = append(out.Sources, src)
	}
	return out, nil
}

// TenantCatalogue reads one tenant's own catalogues and its delegation.
func (g *GitOps) TenantCatalogue(ctx context.Context, tenant string) (TenantCatalogue, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return TenantCatalogue{}, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return TenantCatalogue{}, err
	}
	return parseTenantCatalogue(string(raw))
}

// TenantCatalogues reads every tenant's, by tenant name: what the cluster's
// administrator is shown. A tenant with neither a source nor delegation is
// in the answer with an empty entry.
func (g *GitOps) TenantCatalogues(ctx context.Context) (map[string]TenantCatalogue, error) {
	names, err := g.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TenantCatalogue, len(names))
	for _, name := range names {
		c, err := g.TenantCatalogue(ctx, name)
		if errors.Is(err, ErrTenantNotFound) || errors.Is(err, ErrInvalidName) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[name] = c
	}
	return out, nil
}

// checkCatalogueSource is what every added catalogue must be, whoever adds
// it. The address itself -- that it is public, and what it resolves to -- is
// the caller's to have checked (catalogue.Fetcher.Vet); this is only what
// keeps the manifest a manifest.
func checkCatalogueSource(name, address string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: a catalogue's name is lower-case letters, digits and hyphens", ErrInvalidName)
	}
	if !strings.HasPrefix(address, "https://") || strings.ContainsAny(address, " \t\r\n#\"'") {
		return fmt.Errorf("%w: a catalogue's address is an https URL", ErrInvalidName)
	}
	return nil
}

// AddClusterCatalogueSource names a catalogue every tenant sees, on the
// Cluster claim. The name must not be one any tenant already uses for a
// catalogue of its own: to that tenant the name would then mean two things.
func (g *GitOps) AddClusterCatalogueSource(ctx context.Context, name, address string, meta Meta) (Result, error) {
	if err := checkCatalogueSource(name, address); err != nil {
		return Result{}, err
	}
	tenants, err := g.TenantCatalogues(ctx)
	if err != nil {
		return Result{}, err
	}
	var holders []string
	for tenant, c := range tenants {
		for _, src := range c.Sources {
			if src.Name == name {
				holders = append(holders, tenant)
			}
		}
	}
	if len(holders) > 0 {
		sort.Strings(holders)
		return Result{}, fmt.Errorf("%w: %s is a catalogue of tenant %s; remove it there or choose another name",
			ErrCatalogueNameTaken, name, strings.Join(holders, ", "))
	}
	msg := fmt.Sprintf("feat(cluster): add catalogue %s for every tenant (via %s)", name, meta.actor())
	return g.applyClaim(ctx, msg, meta, func(text string) (string, string, bool, error) {
		have, err := claimCatalogueSources(text)
		if err != nil {
			return text, "", false, err
		}
		for _, src := range have {
			if src.Name != name {
				continue
			}
			if src.URL == address {
				return text, "unchanged", false, nil
			}
			return text, "", false, fmt.Errorf("%w: %s is already a catalogue of this cluster, at %s; remove it first to change its address",
				ErrCatalogueNameTaken, name, src.URL)
		}
		have = append(have, CatalogueSource{Name: name, URL: address})
		out, err := setClaimSources(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "added", true, nil
	})
}

// RemoveClusterCatalogueSource takes a catalogue off the Cluster claim.
// What was installed from it stays installed: the profiles are in the
// deployments repository, and only a new install or a new build needs the
// source.
func (g *GitOps) RemoveClusterCatalogueSource(ctx context.Context, name string, meta Meta) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: catalogue %q", ErrInvalidName, name)
	}
	msg := fmt.Sprintf("feat(cluster): remove catalogue %s (via %s)", name, meta.actor())
	return g.applyClaim(ctx, msg, meta, func(text string) (string, string, bool, error) {
		have, err := claimCatalogueSources(text)
		if err != nil {
			return text, "", false, err
		}
		kept := make([]CatalogueSource, 0, len(have))
		for _, src := range have {
			if src.Name != name {
				kept = append(kept, src)
			}
		}
		if len(kept) == len(have) {
			return text, "", false, fmt.Errorf("%w: %s", ErrCatalogueNotFound, name)
		}
		out, err := setClaimSources(text, kept)
		if err != nil {
			return text, "", false, err
		}
		return out, "removed", true, nil
	})
}

// claimCatalogueSources reads spec.catalogue.sources from the claim's text as
// it is, without dropping what could not be fetched from: an edit rewrites
// the list, and must not lose an entry somebody wrote for being malformed.
func claimCatalogueSources(text string) ([]CatalogueSource, error) {
	var claim struct {
		Spec struct {
			Catalogue struct {
				Sources []CatalogueSource `json:"sources"`
			} `json:"catalogue"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &claim); err != nil {
		return nil, fmt.Errorf("parse cluster claim: %w", err)
	}
	return claim.Spec.Catalogue.Sources, nil
}

func renderClusterSources(sources []CatalogueSource) []string {
	if len(sources) == 0 {
		return []string{"    sources: []"}
	}
	out := []string{"    sources:"}
	for _, src := range sources {
		out = append(out, "      - name: "+quoteScalar(src.Name), "        url: "+plainURL(src.URL))
	}
	return out
}

// plainURL writes an address the way a person would: bare, where that is
// unambiguous YAML, which an address that passed the address check always is.
func plainURL(address string) string {
	if strings.ContainsAny(address, " #\"'{}[]&*!|>@`,") || strings.Contains(address, ": ") || strings.HasSuffix(address, ":") {
		return quoteScalar(address)
	}
	return address
}

// TenantCatalogueActor is who a change to a tenant's catalogues is made by:
// the role the caller was authorised in, never something the caller states.
type TenantCatalogueActor string

// The two roles that change a tenant's catalogues.
const (
	ByCluster TenantCatalogueActor = gentianov1alpha1.CatalogueAddedByCluster
	ByTenant  TenantCatalogueActor = gentianov1alpha1.CatalogueAddedByTenant
)

// AddTenantCatalogueSource names a catalogue only one tenant sees, on that
// tenant's manifest.
//
// by is the role the caller was authorised in. A tenant's administrator may
// add one only where the cluster's administrator delegated that, which is
// read from the same text the entry is written into, so the two cannot
// disagree. The name must not be a catalogue of the whole cluster or another
// of this tenant.
func (g *GitOps) AddTenantCatalogueSource(
	ctx context.Context, tenant, name, address string, by TenantCatalogueActor, meta Meta,
) (Result, error) {
	if err := checkCatalogueSource(name, address); err != nil {
		return Result{}, err
	}
	cluster, err := g.Catalogue(ctx)
	if err != nil && !errors.Is(err, ErrNoClusterClaim) {
		return Result{}, err
	}
	for _, src := range cluster.Sources {
		if src.Name == name {
			return Result{}, fmt.Errorf("%w: %s is a catalogue of the whole cluster, which this tenant already sees; choose another name",
				ErrCatalogueNameTaken, name)
		}
	}
	msg := fmt.Sprintf("feat(%s): add catalogue %s (via %s)", tenant, name, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantCatalogue(text)
		if err != nil {
			return text, "", false, err
		}
		if by == ByTenant && !have.Delegated {
			return text, "", false, ErrCatalogueNotDelegated
		}
		for _, src := range have.Sources {
			if src.Name != name {
				continue
			}
			if src.URL == address && src.AddedBy == string(by) {
				return text, "unchanged", false, nil
			}
			return text, "", false, fmt.Errorf("%w: %s is already a catalogue of this tenant; remove it first to change it",
				ErrCatalogueNameTaken, name)
		}
		have.Sources = append(have.Sources, TenantCatalogueSource{Name: name, URL: address, AddedBy: string(by)})
		out, err := setTenantCatalogue(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "added", true, nil
	})
}

// RemoveTenantCatalogueSource takes one of a tenant's catalogues away. A
// tenant's administrator removes only what the tenant added, and only while
// that is delegated; what the cluster's administrator added is not theirs.
func (g *GitOps) RemoveTenantCatalogueSource(
	ctx context.Context, tenant, name string, by TenantCatalogueActor, meta Meta,
) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: catalogue %q", ErrInvalidName, name)
	}
	msg := fmt.Sprintf("feat(%s): remove catalogue %s (via %s)", tenant, name, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantCatalogue(text)
		if err != nil {
			return text, "", false, err
		}
		if by == ByTenant && !have.Delegated {
			return text, "", false, ErrCatalogueNotDelegated
		}
		kept := make([]TenantCatalogueSource, 0, len(have.Sources))
		found := false
		for _, src := range have.Sources {
			if src.Name != name {
				kept = append(kept, src)
				continue
			}
			found = true
			if by == ByTenant && src.AddedBy != gentianov1alpha1.CatalogueAddedByTenant {
				return text, "", false, ErrCatalogueNotTenants
			}
		}
		if !found {
			return text, "", false, fmt.Errorf("%w: %s", ErrCatalogueNotFound, name)
		}
		have.Sources = kept
		out, err := setTenantCatalogue(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "removed", true, nil
	})
}

// SetTenantCatalogueDelegation turns on or off whether a tenant's own
// administrators may add catalogues. Turning it off removes nothing: what
// they added stays, visible to the tenant and removable by the cluster's
// administrator.
func (g *GitOps) SetTenantCatalogueDelegation(ctx context.Context, tenant string, delegated bool, meta Meta) (Result, error) {
	verb := "delegate adding catalogues to"
	if !delegated {
		verb = "stop delegating catalogues to"
	}
	msg := fmt.Sprintf("feat(%s): %s the tenant's administrators (via %s)", tenant, verb, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantCatalogue(text)
		if err != nil {
			return text, "", false, err
		}
		if have.Delegated == delegated {
			return text, "unchanged", false, nil
		}
		have.Delegated = delegated
		out, err := setTenantCatalogue(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "updated", true, nil
	})
}

// setTenantCatalogue writes spec.catalogue into a tenant manifest's text, or
// removes it when there is nothing to say, and checks that what it wrote
// reads back as what was meant: a line editor that produced something else
// commits nothing.
func setTenantCatalogue(text string, c TenantCatalogue) (string, error) {
	var block []string
	if c.Delegated || len(c.Sources) > 0 {
		block = []string{"  catalogue:"}
		if c.Delegated {
			block = append(block, "    delegated: true")
		}
		if len(c.Sources) > 0 {
			block = append(block, "    sources:")
			for _, src := range c.Sources {
				block = append(block,
					"      - name: "+quoteScalar(src.Name),
					"        url: "+plainURL(src.URL),
					"        addedBy: "+src.AddedBy)
			}
		}
	}
	out, err := setSpecBlock(text, []string{"catalogue"}, block)
	if err != nil {
		return "", err
	}
	back, err := parseTenantCatalogue(out)
	if err != nil {
		return "", fmt.Errorf("the edited manifest is not valid YAML: %w", err)
	}
	if back.Delegated != c.Delegated || len(back.Sources) != len(c.Sources) {
		return "", errors.New("the edited manifest does not read back as written")
	}
	for i := range c.Sources {
		if back.Sources[i] != c.Sources[i] {
			return "", errors.New("the edited manifest does not read back as written")
		}
	}
	return out, nil
}

// setSpecBlock replaces the block at a path of keys under spec with the
// lines given, inserts it where there is none, and removes it when no lines
// are given. Everything outside the block is untouched, byte for byte.
//
// The block is found by indentation, two spaces a level, which is how the
// claim and a tenant's manifest are written -- by the installer, by the
// director, and by a marshalled import alike. Comments and blank lines after
// the block's last line of content are not part of it and stay.
func setSpecBlock(text string, path []string, block []string) (string, error) {
	lines := strings.Split(text, "\n")
	specAt := -1
	for i, l := range lines {
		if indentOf(l) == 0 && (strings.TrimRight(l, " ") == "spec:" || strings.HasPrefix(l, "spec: ")) {
			specAt = i
			break
		}
	}
	if specAt < 0 || strings.TrimRight(lines[specAt], " ") != "spec:" {
		return "", errors.New("the manifest has no spec block")
	}
	// contentEnd is the line after the last line of content in [start, end).
	contentEnd := func(start, end int) int {
		last := start
		for i := start; i < end && i < len(lines); i++ {
			t := strings.TrimSpace(lines[i])
			if t != "" && !strings.HasPrefix(t, "#") {
				last = i + 1
			}
		}
		return last
	}
	// childrenEnd is blockEnd for a key at indent, counting a list written
	// at the key's own indentation -- which is how a marshalled manifest
	// writes one -- as the key's.
	childrenEnd := func(from, indent int) int {
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
	start, end, indent := specAt+1, blockEnd(lines, specAt+1, 0), 2
	for depth, key := range path {
		at := findKeyLine(lines, start, end, indent, key)
		last := depth == len(path)-1
		if last {
			splice := func(from, to int) string {
				out := append([]string{}, lines[:from]...)
				out = append(out, block...)
				return strings.Join(append(out, lines[to:]...), "\n")
			}
			if at < 0 {
				if len(block) == 0 {
					return text, nil
				}
				to := contentEnd(start, end)
				return splice(to, to), nil
			}
			return splice(at, contentEnd(at+1, childrenEnd(at+1, indent))), nil
		}
		if at < 0 {
			if len(block) == 0 {
				return text, nil
			}
			// A parent that is not there yet is written, at the end of
			// what its own parent holds.
			to := contentEnd(start, end)
			lines = append(lines[:to:to], append([]string{strings.Repeat(" ", indent) + key + ":"}, lines[to:]...)...)
			at = to
		} else if value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[at]), key+":")); value != "" && !strings.HasPrefix(value, "#") {
			// `catalogue: {}` is how a claim says "none", and it can hold
			// children once it is a block. Anything else on the line is a
			// value this editor would be writing children under.
			if value != "{}" {
				return "", ErrCatalogueFlowForm
			}
			lines[at] = strings.Repeat(" ", indent) + key + ":"
		}
		start, end = at+1, childrenEnd(at+1, indent)
		indent += 2
	}
	return text, nil
}

// setClaimSources writes spec.catalogue.sources into the claim's text and
// checks that it reads back as what was meant.
func setClaimSources(text string, sources []CatalogueSource) (string, error) {
	out, err := setSpecBlock(text, []string{"catalogue", "sources"}, renderClusterSources(sources))
	if err != nil {
		return "", err
	}
	back, err := claimCatalogueSources(out)
	if err != nil {
		return "", fmt.Errorf("the edited claim is not valid YAML: %w", err)
	}
	if len(back) != len(sources) {
		return "", errors.New("the edited claim does not read back as written")
	}
	for i := range sources {
		if back[i] != sources[i] {
			return "", errors.New("the edited claim does not read back as written")
		}
	}
	return out, nil
}
