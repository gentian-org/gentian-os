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
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
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
// what the cluster offers, not a license: an install is asked of the person
// (can_install_app), and whether the app then arrives is decided where its
// artifacts are pulled.
type CatalogueSource struct {
	// Name is the catalog's slug: the first half of a coordinate, so
	// "main/nextcloud-base-ce" is served by the source named main.
	Name string `json:"name"`
	// URL is the https address bundles are fetched from.
	URL string `json:"url,omitempty"`
	// OCI is an OCI artifact the catalog is kept as, read with the
	// credential of a repository declared for the cluster (catalogue/oci.go).
	// A source states one of URL and OCI. A catalog is never kept in the
	// deployments repository: that holds the cluster's state only.
	OCI CatalogueOCI `json:"oci,omitempty"`
	// AddedBy is SourceAddedByStore for a source the installer added
	// because the store the claim names lists it, and empty for one the
	// cluster's administrator added. It is kept through every edit of the
	// list and decides nothing: the source is the cluster's like any other.
	AddedBy string `json:"addedBy,omitempty"`
}

// SourceAddedByStore marks a catalog of the whole cluster the installer
// added from the store's GET /v1/meta (scripts/lib/catalogue.sh, ensure_claim_store_catalogues).
const SourceAddedByStore = "store"

// sameSource says two sources are read from the same place, whoever added
// them.
func sameSource(a, b CatalogueSource) bool {
	return a.Name == b.Name && a.URL == b.URL && a.OCI == b.OCI
}

// CatalogueOCI is a catalog kept as an OCI artifact: the repository whose
// credential it is read with, and the artifact. Zero is none.
type CatalogueOCI struct {
	Repository string `json:"repository,omitempty"`
	Artifact   string `json:"artifact,omitempty"`
}

// Set says the source states an artifact at all.
func (o CatalogueOCI) Set() bool { return o != CatalogueOCI{} }

// CheckCatalogueOCI says what is wrong with an artifact a catalog is
// declared as, as it is written: the repository's name and the reference. It
// resolves nothing and reads no repository.
func CheckCatalogueOCI(o CatalogueOCI) error {
	if !ValidName(o.Repository) {
		return fmt.Errorf("%w: an artifact names the repository it is read with: lower-case letters, digits and hyphens", catalogue.ErrRegistryRefused)
	}
	_, err := catalogue.ParseOCIReference(o.Artifact)
	return err
}

// where says where a source is read from, for a message.
func (c CatalogueSource) where() string {
	return where(c.URL, c.OCI)
}

func where(address string, oci CatalogueOCI) string {
	if oci.Set() {
		return "the artifact " + oci.Artifact + " (repository " + oci.Repository + ")"
	}
	return address
}

// usableSource says whether a declared source is one that is read from: an
// https address, or an artifact and the repository it is read with -- one of
// them, not both.
func usableSource(address string, oci CatalogueOCI) bool {
	switch {
	case address != "" && oci.Set():
		return false
	case oci.Set():
		return CheckCatalogueOCI(oci) == nil
	}
	return strings.HasPrefix(address, "https://")
}

// trimOCI is an artifact as it is compared: without surrounding whitespace.
func trimOCI(o CatalogueOCI) CatalogueOCI {
	return CatalogueOCI{Repository: strings.TrimSpace(o.Repository), Artifact: strings.TrimSpace(o.Artifact)}
}

// CatalogueSettings is the claim's whole spec.catalogue.
type CatalogueSettings struct {
	// StoreURL is where people are sent to get the entries this cluster
	// does not list itself -- everything maintained or licensed by somebody.
	//
	// Here rather than compiled in, because which store a cluster belongs to
	// is a fact about that cluster, and because a cluster with no store at
	// all is a supported thing: it says nothing and its own catalogs are
	// all there is.
	StoreURL string `json:"storeUrl,omitempty"`
	// Sources are the catalogs this cluster may fetch from.
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
		src.OCI = trimOCI(src.OCI)
		if src.Name == "" || !usableSource(src.URL, src.OCI) {
			// http:// is skipped here as it is everywhere else: the digest
			// makes the bytes safe, but a cluster fetching its catalog in
			// clear announces what it runs. So is an entry that states both
			// an address and an artifact, or neither.
			continue
		}
		out.Sources = append(out.Sources, src)
	}
	return out, nil
}

// CatalogueSources reads the sources this cluster declares.
//
// A source with no name, or with neither an https URL nor an artifact, is
// skipped rather than refused: the
// claim is read on every start, and one malformed entry must not stop a
// director from serving. What it costs is that installs from that catalog
// are refused for want of a source, which says the same thing where somebody
// will see it.
func (g *GitOps) CatalogueSources(ctx context.Context) ([]CatalogueSource, error) {
	c, err := g.Catalogue(ctx)
	if err != nil {
		return nil, err
	}
	return c.Sources, nil
}

// A tenant's own catalogs, and who may add them.
//
// On the tenant's manifest, for the reason the cluster's are on the claim: a
// catalogue is where software enters, and adding one is a commit with an
// author. Three things are recorded -- the sources only this tenant sees, who
// added each, and whether the tenant's own administrators may add any at all
// -- and the last two are the cluster administrator's alone to set. They are
// written by the methods below and by nothing else: no other write to a
// tenant's manifest touches spec.catalogue, and which of these methods a
// caller reaches is decided by the route it was authorized on.

// TenantCatalogueSource is one catalog only a tenant sees.
type TenantCatalogueSource struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
	// OCI is an OCI artifact, in place of an address, read with the
	// credential of a repository declared for this tenant.
	OCI CatalogueOCI `json:"oci,omitempty"`
	// AddedBy is "cluster" or "tenant". Anything else in the manifest is
	// read as "cluster": what nobody is known to have added, the tenant's
	// administrators may not remove.
	AddedBy string `json:"addedBy"`
}

// TenantCatalogue is a tenant's spec.catalogue.
type TenantCatalogue struct {
	// Delegated says the tenant's own administrators may add and remove
	// catalogs for this tenant.
	Delegated bool                    `json:"delegated"`
	Sources   []TenantCatalogueSource `json:"sources"`
}

// Errors of declaring a catalog.
var (
	// ErrCatalogueNameTaken is a name that already means another catalog
	// to the tenants that would see this one.
	ErrCatalogueNameTaken = errors.New("the catalog name is taken")
	// ErrCatalogueNotFound is a catalog that is not there to remove, or
	// not the caller's to remove.
	ErrCatalogueNotFound = errors.New("no such catalog")
	// ErrCatalogueNotDelegated is a tenant whose own administrators may not
	// add catalogs.
	ErrCatalogueNotDelegated = errors.New("adding catalogs is not delegated to this tenant")
	// ErrCatalogueNotTenants is a catalog the cluster's administrator
	// added, which the tenant's may not remove.
	ErrCatalogueNotTenants = errors.New("the catalog was added by the cluster's administrator")
	// ErrCatalogueFlowForm is a catalog section written in a form the
	// line editor cannot change.
	ErrCatalogueFlowForm = errors.New("the catalog section is written on one line; write it as a block to change it here")
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
		src.OCI = trimOCI(src.OCI)
		if !ValidName(src.Name) || !usableSource(src.URL, src.OCI) || seen[src.Name] {
			continue
		}
		if src.AddedBy != gentianov1alpha1.CatalogueAddedByTenant {
			src.AddedBy = gentianov1alpha1.CatalogueAddedByCluster
		}
		seen[src.Name] = true
		out.Sources = append(out.Sources, src)
	}
	return out, nil
}

// TenantCatalogue reads one tenant's own catalogs and its delegation.
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
		return fmt.Errorf("%w: a catalog's name is lower-case letters, digits and hyphens", ErrInvalidName)
	}
	if !strings.HasPrefix(address, "https://") || strings.ContainsAny(address, " \t\r\n#\"'") {
		return fmt.Errorf("%w: a catalog's address is an https URL", ErrInvalidName)
	}
	return nil
}

// AddClusterCatalogueSource names a catalog every tenant sees, on the
// Cluster claim. The name must not be one any tenant already uses for a
// catalogue of its own: to that tenant the name would then mean two things.
func (g *GitOps) AddClusterCatalogueSource(ctx context.Context, name, address string, meta Meta) (Result, error) {
	if err := checkCatalogueSource(name, address); err != nil {
		return Result{}, err
	}
	return g.addClusterSource(ctx, CatalogueSource{Name: name, URL: address}, meta)
}

// AddClusterCatalogueOCI names a catalog every tenant sees that is kept as
// an OCI artifact, read with the credential of a repository declared for the
// cluster. That the repository is declared, and that the artifact lies inside
// it, is the caller's to have asked (RegistryRepository); it is asked again
// on every pull.
func (g *GitOps) AddClusterCatalogueOCI(ctx context.Context, name string, oci CatalogueOCI, meta Meta) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: a catalog's name is lower-case letters, digits and hyphens", ErrInvalidName)
	}
	if err := CheckCatalogueOCI(oci); err != nil {
		return Result{}, err
	}
	return g.addClusterSource(ctx, CatalogueSource{Name: name, OCI: oci}, meta)
}

func (g *GitOps) addClusterSource(ctx context.Context, want CatalogueSource, meta Meta) (Result, error) {
	name := want.Name
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
		return Result{}, fmt.Errorf("%w: %s is a catalog of tenant %s; remove it there or choose another name",
			ErrCatalogueNameTaken, name, strings.Join(holders, ", "))
	}
	msg := fmt.Sprintf("feat(cluster): add catalog %s for every tenant (via %s)", name, meta.actor())
	return g.applyClaim(ctx, msg, meta, func(text string) (string, string, bool, error) {
		have, err := claimCatalogueSources(text)
		if err != nil {
			return text, "", false, err
		}
		for _, src := range have {
			if src.Name != name {
				continue
			}
			if sameSource(src, want) {
				return text, "unchanged", false, nil
			}
			return text, "", false, fmt.Errorf("%w: %s is already a catalog of this cluster, at %s; remove it first to change its address",
				ErrCatalogueNameTaken, name, src.where())
		}
		have = append(have, want)
		out, err := setClaimSources(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "added", true, nil
	})
}

// RemoveClusterCatalogueSource takes a catalog off the Cluster claim.
// What was installed from it stays installed: the profiles are in the
// deployments repository, and only a new install or a new build needs the
// source.
func (g *GitOps) RemoveClusterCatalogueSource(ctx context.Context, name string, meta Meta) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: catalog %q", ErrInvalidName, name)
	}
	msg := fmt.Sprintf("feat(cluster): remove catalog %s (via %s)", name, meta.actor())
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
		out = append(out, "      - name: "+quoteScalar(src.Name))
		out = append(out, sourceLines(src.URL, src.OCI)...)
		if src.AddedBy != "" {
			out = append(out, "        addedBy: "+quoteScalar(src.AddedBy))
		}
	}
	return out
}

// sourceLines are the lines of a source that say where it is read from: its
// address or its artifact. A source somebody wrote by hand with both, or with
// neither, is not read from; it is written back as it was, so that an edit of
// the list loses nothing.
func sourceLines(address string, oci CatalogueOCI) []string {
	var out []string
	if address != "" || !oci.Set() {
		out = append(out, "        url: "+plainURL(address))
	}
	if oci.Set() {
		out = append(out, "        oci:",
			"          repository: "+pathScalar(oci.Repository),
			"          artifact: "+pathScalar(oci.Artifact))
	}
	return out
}

// pathScalar writes a repository's name or an artifact bare where YAML reads
// that back as the same text, and quoted where it would read it as something
// else: a name such as on, 2024 or null is a name, not a value.
func pathScalar(v string) string {
	var back struct {
		V any `json:"v"`
	}
	if err := yaml.Unmarshal([]byte("v: "+v+"\n"), &back); err == nil && back.V == v {
		return v
	}
	return strconv.Quote(v)
}

// plainURL writes an address the way a person would: bare, where that is
// unambiguous YAML, which an address that passed the address check always is.
func plainURL(address string) string {
	if strings.ContainsAny(address, " #\"'{}[]&*!|>@`,") || strings.Contains(address, ": ") || strings.HasSuffix(address, ":") {
		return quoteScalar(address)
	}
	return address
}

// TenantCatalogueActor is who a change to a tenant's catalogs is made by:
// the role the caller was authorized in, never something the caller states.
type TenantCatalogueActor string

// The two roles that change a tenant's catalogs.
const (
	ByCluster TenantCatalogueActor = gentianov1alpha1.CatalogueAddedByCluster
	ByTenant  TenantCatalogueActor = gentianov1alpha1.CatalogueAddedByTenant
)

// AddTenantCatalogueSource names a catalog only one tenant sees, on that
// tenant's manifest.
//
// by is the role the caller was authorized in. A tenant's administrator may
// add one only where the cluster's administrator delegated that, which is
// read from the same text the entry is written into, so the two cannot
// disagree. The name must not be a catalog of the whole cluster or another
// of this tenant.
func (g *GitOps) AddTenantCatalogueSource(
	ctx context.Context, tenant, name, address string, by TenantCatalogueActor, meta Meta,
) (Result, error) {
	if err := checkCatalogueSource(name, address); err != nil {
		return Result{}, err
	}
	return g.addTenantSource(ctx, tenant, TenantCatalogueSource{Name: name, URL: address, AddedBy: string(by)}, meta)
}

// AddTenantCatalogueOCI names a catalog only one tenant sees that is kept
// as an OCI artifact, read with the credential of a repository declared for
// that tenant. by is the role the caller was authorized in, as for an
// address: a tenant's administrator adds one where that is delegated.
func (g *GitOps) AddTenantCatalogueOCI(
	ctx context.Context, tenant, name string, oci CatalogueOCI, by TenantCatalogueActor, meta Meta,
) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: a catalog's name is lower-case letters, digits and hyphens", ErrInvalidName)
	}
	if err := CheckCatalogueOCI(oci); err != nil {
		return Result{}, err
	}
	return g.addTenantSource(ctx, tenant, TenantCatalogueSource{Name: name, OCI: oci, AddedBy: string(by)}, meta)
}

func (g *GitOps) addTenantSource(ctx context.Context, tenant string, want TenantCatalogueSource, meta Meta) (Result, error) {
	name, by := want.Name, TenantCatalogueActor(want.AddedBy)
	cluster, err := g.Catalogue(ctx)
	if err != nil && !errors.Is(err, ErrNoClusterClaim) {
		return Result{}, err
	}
	for _, src := range cluster.Sources {
		if src.Name == name {
			return Result{}, fmt.Errorf("%w: %s is a catalog of the whole cluster, which this tenant already sees; choose another name",
				ErrCatalogueNameTaken, name)
		}
	}
	msg := fmt.Sprintf("feat(%s): add catalog %s (via %s)", tenant, name, meta.actor())
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
			if src == want {
				return text, "unchanged", false, nil
			}
			return text, "", false, fmt.Errorf("%w: %s is already a catalog of this tenant; remove it first to change it",
				ErrCatalogueNameTaken, name)
		}
		have.Sources = append(have.Sources, want)
		out, err := setTenantCatalogue(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "added", true, nil
	})
}

// RemoveTenantCatalogueSource takes one of a tenant's catalogs away. A
// tenant's administrator removes only what the tenant added, and only while
// that is delegated; what the cluster's administrator added is not theirs.
func (g *GitOps) RemoveTenantCatalogueSource(
	ctx context.Context, tenant, name string, by TenantCatalogueActor, meta Meta,
) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: catalog %q", ErrInvalidName, name)
	}
	msg := fmt.Sprintf("feat(%s): remove catalog %s (via %s)", tenant, name, meta.actor())
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
// administrators may add catalogs. Turning it off removes nothing: what
// they added stays, visible to the tenant and removable by the cluster's
// administrator.
func (g *GitOps) SetTenantCatalogueDelegation(ctx context.Context, tenant string, delegated bool, meta Meta) (Result, error) {
	verb := "delegate adding catalogs to"
	if !delegated {
		verb = "stop delegating catalogs to"
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
				block = append(block, "      - name: "+quoteScalar(src.Name))
				block = append(block, sourceLines(src.URL, src.OCI)...)
				block = append(block, "        addedBy: "+src.AddedBy)
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
// director, and by a marshaled import alike. Comments and blank lines after
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
	// at the key's own indentation -- which is how a marshaled manifest
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
