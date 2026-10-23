/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/platformversion"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// What a cluster can say about its own catalogues, and what it deliberately
// cannot (AD-14).
//
// The App Store is the route. It knows what an app is for, what it costs, who
// maintains it and what the screenshots look like, and it is kept current by
// people whose job that is. This is what remains when the store is not the
// answer: the store is unreachable, or the cluster is air-gapped, or -- the
// case that has no store answer at all -- the entry is the operator's own
// profile in the operator's own repository, which nobody sells and nobody
// else lists.
//
// So it lists coordinates, versions, editions and digests, and nothing that
// would pretend to be a shop. Only ce, me and ie are listed: pe and ee are a
// supplier's proprietary products, and an entry whose whole value is a
// relationship with a supplier is not something a cluster can describe
// usefully, so it is counted and named to the store instead.
//
// None of this is a license check. A source named on the Cluster claim is
// offered to every tenant; whether an install is allowed is the person's
// can_install_app and nothing else.
//
// Which catalogues there are is read when it is asked, from git, and it
// depends on who is asking for which tenant. A catalogue exists in one of
// three ways:
//
//   - for the whole cluster, on the Cluster claim: every tenant sees it;
//   - for one tenant, added by the cluster's administrator: only that tenant
//     sees it;
//   - for one tenant, added by that tenant's own administrator -- possible
//     only where the cluster's administrator delegated it to that tenant.
//
// And it is read from one of two places: an https address, or an OCI
// artifact read with the credential of a repository its owner declared
// (catalogue/oci.go). An artifact is added by whoever may add an address, and
// is read with the credential of the owner's repository only: the cluster's
// for a catalogue of the cluster, the tenant's for one of a tenant. It
// changes where the bytes come from and nothing else -- a catalogue kept as
// an artifact is a catalogue of the cluster or of a tenant exactly as one at
// an address is. A catalogue is never kept in the deployments repository,
// which holds the cluster's state only.
//
// To a tenant, another tenant's catalogue does not exist: it is not listed,
// a coordinate that names it is refused in the words an unknown catalogue is
// refused in, and nothing it serves can be installed anywhere else.

// Scopes of a catalogue, as the lists name them.
const (
	scopeCluster = "cluster"
	scopeTenant  = "tenant"
)

type catalogueOut struct {
	Name string `json:"name"`
	// URL is the address it is served from; empty for a catalogue kept as
	// an OCI artifact.
	URL string `json:"url"`
	// OCI is the artifact it is kept as, and the repository it is read
	// with, for a catalogue that has no address.
	OCI *ociOut `json:"oci,omitempty"`
	// Scope is "cluster" for a catalogue every tenant sees and "tenant" for
	// one only this tenant does.
	Scope string `json:"scope"`
	// AddedBy is who added it: "cluster" for the cluster's administrator,
	// "tenant" for the tenant's own, "store" for a catalogue of the whole
	// cluster the installer added because the cluster's store lists it.
	AddedBy string `json:"addedBy"`
}

type ociOut struct {
	Repository string `json:"repository"`
	Artifact   string `json:"artifact"`
}

// ociOutOf is an artifact as the lists show it; nil for a catalogue that is
// none.
func ociOutOf(o gitops.CatalogueOCI) *ociOut {
	if !o.Set() {
		return nil
	}
	return &ociOut{Repository: o.Repository, Artifact: o.Artifact}
}

// ociSource is an artifact as it is pulled: with the owner whose repository
// it is read with, which is where it is declared and nothing it says.
func ociSource(owner string, o gitops.CatalogueOCI) *catalogue.OCISource {
	if !o.Set() {
		return nil
	}
	return &catalogue.OCISource{Owner: owner, Repository: o.Repository, Artifact: o.Artifact}
}

type entryOut struct {
	Coordinate string `json:"coordinate"`
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Edition    string `json:"edition"`
	TrustTier  string `json:"trustTier,omitempty"`
	// Digest is the build the source lists: what an install of this entry
	// from here sends back.
	Digest string `json:"digest,omitempty"`
	// Platform is the floor the build declares, spec.requires.platform, as
	// the index repeats it.
	Platform string `json:"platform,omitempty"`
	// Unmet is why this cluster cannot install the entry although it is
	// listed: "needs platform 0.6; this cluster is 0.5". Empty when it can.
	Unmet string `json:"unmet,omitempty"`
	// Installable says this entry can be installed from this list: it states
	// its digest, which is what an install from a source is pinned to, and
	// its floor is not above this platform. False sends the person to the
	// store, or says what the cluster lacks.
	Installable bool `json:"installable"`
}

// visibleSource is one catalogue as a tenant sees it.
type visibleSource struct {
	catalogue.Source
	scope   string
	addedBy string
}

// origin is what a profile fetched from this catalogue is recorded as
// coming from.
func (v visibleSource) origin(tenant string) string {
	if v.scope == scopeTenant {
		return profilebundle.TenantOrigin(tenant, v.Name)
	}
	return profilebundle.ClusterOrigin(v.Name)
}

// visibleSources reads what a tenant may install from: the cluster's
// catalogues and its own, in that order, and whether its administrators may
// add more. Read from git every time it is asked, so a catalogue added a
// moment ago is there and one removed is gone.
//
// A name is one catalogue. The routes that add catalogues keep a tenant's
// names apart from the cluster's; should the two files ever disagree, the
// name is left out here and so refused everywhere, rather than meaning one
// catalogue when it was pinned and another when it is fetched.
func (s *Server) visibleSources(ctx context.Context, tenant string) ([]visibleSource, bool, error) {
	cluster, err := s.cfg.Repo.Catalogue(ctx)
	if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
		return nil, false, err
	}
	own, err := s.cfg.Repo.TenantCatalogue(ctx, tenant)
	if err != nil {
		return nil, false, err
	}
	twice := map[string]bool{}
	names := map[string]bool{}
	for _, src := range cluster.Sources {
		names[src.Name] = true
	}
	for _, src := range own.Sources {
		if names[src.Name] {
			twice[src.Name] = true
		}
	}
	out := make([]visibleSource, 0, len(cluster.Sources)+len(own.Sources))
	for _, src := range cluster.Sources {
		if twice[src.Name] {
			continue
		}
		out = append(out, clusterSource(src))
	}
	for _, src := range own.Sources {
		if twice[src.Name] {
			continue
		}
		out = append(out, visibleSource{
			Source: catalogue.Source{
				Key: profilebundle.TenantOrigin(tenant, src.Name), Name: src.Name, URL: src.URL,
				OCI: ociSource(tenant, src.OCI),
			},
			scope: scopeTenant, addedBy: src.AddedBy,
		})
	}
	return out, own.Delegated, nil
}

// clusterSource is one of the cluster's catalogues as it is fetched from:
// an address or an artifact, read with a repository of the cluster's. Every
// reader of the cluster's catalogues builds its source here, so none of them
// knows fewer kinds of catalogue than the others.
func clusterSource(src gitops.CatalogueSource) visibleSource {
	return visibleSource{
		Source: catalogue.Source{
			Key: profilebundle.ClusterOrigin(src.Name), Name: src.Name, URL: src.URL,
			OCI: ociSource("", src.OCI),
		},
		scope: scopeCluster, addedBy: clusterAddedBy(src),
	}
}

// clusterAddedBy is who added one of the cluster's catalogues, as the lists
// say it.
func clusterAddedBy(src gitops.CatalogueSource) string {
	if src.AddedBy == gitops.SourceAddedByStore {
		return gitops.SourceAddedByStore
	}
	return string(gitops.ByCluster)
}

// resolveSource answers the catalogue a name means to a tenant, or false.
func (s *Server) resolveSource(ctx context.Context, tenant, name string) (visibleSource, bool, error) {
	sources, _, err := s.visibleSources(ctx, tenant)
	if err != nil {
		return visibleSource{}, false, err
	}
	for _, src := range sources {
		if src.Name == name {
			return src, true, nil
		}
	}
	return visibleSource{}, false, nil
}

// listCatalogues answers what this tenant could install from at all.
func (s *Server) listCatalogues(w http.ResponseWriter, r *http.Request, _ call) {
	tenant := r.PathValue("t")
	sources, delegated, err := s.visibleSources(r.Context(), tenant)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	out := make([]catalogueOut, 0, len(sources))
	for _, src := range sources {
		var oci *ociOut
		if src.OCI != nil {
			oci = &ociOut{Repository: src.OCI.Repository, Artifact: src.OCI.Artifact}
		}
		out = append(out, catalogueOut{Name: src.Name, URL: src.URL, OCI: oci, Scope: src.scope, AddedBy: src.addedBy})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "storeUrl": s.cfg.StoreURL, "delegated": delegated, "catalogues": out,
	})
}

// listClusterCatalogues answers everything the cluster declares: its own
// catalogues, and for every tenant the ones only it sees and whether its
// administrators may add them.
func (s *Server) listClusterCatalogues(w http.ResponseWriter, r *http.Request, _ call) {
	ctx := r.Context()
	cluster, err := s.cfg.Repo.Catalogue(ctx)
	if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
		s.repoError(w, r, err)
		return
	}
	own := make([]catalogueOut, 0, len(cluster.Sources))
	for _, src := range cluster.Sources {
		own = append(own, catalogueOut{
			Name: src.Name, URL: src.URL, OCI: ociOutOf(src.OCI), Scope: scopeCluster, AddedBy: clusterAddedBy(src),
		})
	}
	perTenant, err := s.cfg.Repo.TenantCatalogues(ctx)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	type tenantOut struct {
		Tenant     string         `json:"tenant"`
		Delegated  bool           `json:"delegated"`
		Catalogues []catalogueOut `json:"catalogues"`
	}
	tenants := make([]tenantOut, 0, len(perTenant))
	for name, c := range perTenant {
		entry := tenantOut{Tenant: name, Delegated: c.Delegated, Catalogues: make([]catalogueOut, 0, len(c.Sources))}
		for _, src := range c.Sources {
			entry.Catalogues = append(entry.Catalogues, catalogueOut{
				Name: src.Name, URL: src.URL, OCI: ociOutOf(src.OCI), Scope: scopeTenant, AddedBy: src.AddedBy,
			})
		}
		tenants = append(tenants, entry)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].Tenant < tenants[j].Tenant })
	s.json(w, http.StatusOK, map[string]any{
		"cluster": s.cfg.Cluster, "storeUrl": s.cfg.StoreURL, "catalogues": own, "tenants": tenants,
	})
}

// listCatalogueEntries answers what is in one of them.
func (s *Server) listCatalogueEntries(w http.ResponseWriter, r *http.Request, _ call) {
	tenant, name := r.PathValue("t"), r.PathValue("s")
	src, known, err := s.resolveSource(r.Context(), tenant, name)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if !known || s.cfg.Catalogue == nil {
		s.fail(w, r, http.StatusNotFound, "this cluster has no catalogue by that name")
		return
	}
	listing, err := s.cfg.Catalogue.Index(r.Context(), src.Source)
	switch {
	case errors.Is(err, catalogue.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "this cluster has no catalogue by that name")
		return
	case errors.Is(err, catalogue.ErrRegistryRefused), errors.Is(err, catalogue.ErrArtifactRefused):
		// An artifact that is not a catalogue, or a repository it can no
		// longer be read with. The owner has to act, so the reason is said.
		s.cfg.Log.WarnContext(r.Context(), "a catalogue artifact could not be read",
			"request_id", reqID(r.Context()), "catalogue", src.Key, "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the catalogue "+src.Name+" could not be read: "+err.Error())
		return
	case err != nil:
		// The source is somebody else's web server and it is allowed to be
		// down. Said as a gateway error rather than a server error, because
		// nothing here is broken.
		s.cfg.Log.WarnContext(r.Context(), "a catalogue source's index could not be read",
			"request_id", reqID(r.Context()), "catalogue", src.Key, "error", err)
		s.fail(w, r, http.StatusBadGateway, "the catalogue source could not be read")
		return
	}
	entries := make([]entryOut, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		unmet := platformversion.Unmet(e.Platform)
		entries = append(entries, entryOut{
			Coordinate: name + "/" + e.Name,
			Name:       e.Name,
			Version:    e.Version,
			Edition:    string(e.Edition),
			TrustTier:  e.TrustTier,
			Digest:     e.Digest,
			Platform:   e.Platform,
			Unmet:      unmet,
			// Without a digest there is nothing to pin an install to, and
			// the store is the route; a floor above this platform is said,
			// and the install would be refused.
			Installable: e.Digest != "" && unmet == "",
		})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "catalogue": name, "scope": src.scope,
		"storeUrl": s.cfg.StoreURL, "entries": entries, "storeOnly": len(listing.StoreOnly),
	})
}

// Adding and removing catalogues.
//
// Every one of these is a commit: to the Cluster claim for a catalogue of
// the whole cluster, to the tenant's manifest for one of a tenant. Who made
// it is the commit's author, and the role they made it in is the route's --
// a tenant's administrator reaches only the two tenant routes, which add and
// remove with addedBy: tenant and are refused unless the tenant is delegated.
// Nothing in a request body says whose a catalogue is or who added it.

// catalogueRequest says where the catalogue is: at an https address, or as
// an OCI artifact read with a declared repository. One of the two.
type catalogueRequest struct {
	URL string               `json:"url"`
	OCI *gitops.CatalogueOCI `json:"oci"`
}

// vetted reads where a request says the catalogue is and checks it.
//
// An address is checked as it is written and by what its host resolves to,
// whoever sends it -- the cluster's administrator's typo must not make this
// process probe the cluster either. A catalogue is never a directory of the
// deployments repository, which holds the cluster's state only: a body that
// names one (path) is refused as any other unknown field is.
//
// owner is whose repositories an artifact may be read with: the tenant the
// route names, or "" on the cluster's own route. It is the route's, never the
// body's. An artifact is checked as it is written, by what its registry's
// host resolves to, and against the repository it names: one owner
// declared, a registry, whose address the artifact lies inside. Whether the
// credential is set is not asked here, so that a repository can be declared,
// the catalogue added and the credential set in any order; it is asked on
// every pull.
func (s *Server) vetted(w http.ResponseWriter, r *http.Request, owner string) (catalogueRequest, bool) {
	var body catalogueRequest
	err := decode(r, &body)
	if err != nil || (body.URL != "") == (body.OCI != nil) {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"url": "https://<host>/<path>"} or {"oci": {"repository": "<declared repository>", "artifact": "<host>/<path>:<tag>"}}; `+
				`a catalogue is never kept in the deployments repository, which holds the cluster's state only`)
		return catalogueRequest{}, false
	}
	if s.cfg.Catalogue == nil || s.cfg.Catalogue.Vet == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "this director fetches from no catalogue, so none can be added")
		return catalogueRequest{}, false
	}
	if body.OCI != nil {
		return s.vettedOCI(w, r, *body.OCI, owner)
	}
	if err := s.cfg.Catalogue.Vet(r.Context(), body.URL); err != nil {
		s.catalogueError(w, r, r.PathValue("s"), err)
		return catalogueRequest{}, false
	}
	return catalogueRequest{URL: strings.TrimSuffix(body.URL, "/")}, true
}

// vettedOCI is vetted for an artifact.
func (s *Server) vettedOCI(w http.ResponseWriter, r *http.Request, oci gitops.CatalogueOCI, owner string) (catalogueRequest, bool) {
	name := r.PathValue("s")
	if s.cfg.Catalogue.Registries == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "this director reads no repository credential, so no catalogue kept as an OCI artifact can be added")
		return catalogueRequest{}, false
	}
	if err := gitops.CheckCatalogueOCI(oci); err != nil {
		s.catalogueError(w, r, name, err)
		return catalogueRequest{}, false
	}
	ref, _ := catalogue.ParseOCIReference(oci.Artifact)
	if err := s.cfg.Catalogue.Vet(r.Context(), "https://"+ref.Host+"/"); err != nil {
		s.catalogueError(w, r, name, err)
		return catalogueRequest{}, false
	}
	declared, err := s.cfg.Repo.RegistryRepository(r.Context(), owner, oci.Repository)
	if err == nil {
		err = catalogue.CheckArtifactInRegistry(ref, declared.URL)
	}
	if err != nil {
		s.catalogueError(w, r, name, err)
		return catalogueRequest{}, false
	}
	return catalogueRequest{OCI: &oci}, true
}

func (s *Server) addClusterCatalogue(w http.ResponseWriter, r *http.Request, c call) {
	where, ok := s.vetted(w, r, "")
	if !ok {
		return
	}
	var res gitops.Result
	var err error
	if where.OCI != nil {
		res, err = s.cfg.Repo.AddClusterCatalogueOCI(r.Context(), r.PathValue("s"), *where.OCI, c.meta)
	} else {
		res, err = s.cfg.Repo.AddClusterCatalogueSource(r.Context(), r.PathValue("s"), where.URL, c.meta)
	}
	s.catalogueWritten(w, r, res, err, scopeCluster, "")
}

func (s *Server) removeClusterCatalogue(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveClusterCatalogueSource(r.Context(), r.PathValue("s"), c.meta)
	s.catalogueWritten(w, r, res, err, scopeCluster, "")
}

// addTenantCatalogueAsCluster and addTenantCatalogue differ in one thing,
// which is the whole difference between the two routes: who the entry says
// added it, taken from the route and from nowhere else.
func (s *Server) addTenantCatalogueAsCluster(w http.ResponseWriter, r *http.Request, c call) {
	s.addTenantCatalogueBy(w, r, c, gitops.ByCluster)
}

func (s *Server) addTenantCatalogue(w http.ResponseWriter, r *http.Request, c call) {
	s.addTenantCatalogueBy(w, r, c, gitops.ByTenant)
}

func (s *Server) addTenantCatalogueBy(w http.ResponseWriter, r *http.Request, c call, by gitops.TenantCatalogueActor) {
	tenant := r.PathValue("t")
	if s.refusedInPlatformTenant(w, r, tenant) {
		return
	}
	if by == gitops.ByTenant {
		// Asked before the address is resolved: a tenant that is not
		// delegated must not be able to make this process look anything up.
		// The write checks it again, on the text it edits.
		own, err := s.cfg.Repo.TenantCatalogue(r.Context(), tenant)
		if err != nil {
			s.repoError(w, r, err)
			return
		}
		if !own.Delegated {
			s.catalogueError(w, r, r.PathValue("s"), gitops.ErrCatalogueNotDelegated)
			return
		}
	}
	where, ok := s.vetted(w, r, tenant)
	if !ok {
		return
	}
	var res gitops.Result
	var err error
	if where.OCI != nil {
		// Read with a repository of this tenant's, whoever adds it: the
		// owner is the route's tenant (vetted), and so is the manifest.
		res, err = s.cfg.Repo.AddTenantCatalogueOCI(r.Context(), tenant, r.PathValue("s"), *where.OCI, by, c.meta)
	} else {
		res, err = s.cfg.Repo.AddTenantCatalogueSource(r.Context(), tenant, r.PathValue("s"), where.URL, by, c.meta)
	}
	s.catalogueWritten(w, r, res, err, scopeTenant, tenant)
}

func (s *Server) removeTenantCatalogueAsCluster(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveTenantCatalogueSource(r.Context(), r.PathValue("t"), r.PathValue("s"), gitops.ByCluster, c.meta)
	s.catalogueWritten(w, r, res, err, scopeTenant, r.PathValue("t"))
}

func (s *Server) removeTenantCatalogue(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveTenantCatalogueSource(r.Context(), r.PathValue("t"), r.PathValue("s"), gitops.ByTenant, c.meta)
	s.catalogueWritten(w, r, res, err, scopeTenant, r.PathValue("t"))
}

// Renaming a catalogue.
//
// A catalogue's name is in three places: its source entry, the catalogue:
// of every app entry and add-on pin installed from it, and the origin of
// every profile materialised from it. Removing it and adding it under
// another name leaves the last two at the old name, and the next install
// from the new one is refused as another origin's. So a rename is one
// commit to all three (gitops.RenameCatalogueSource), by the cluster's
// administrator, for a catalogue of the cluster or of a tenant alike: what
// it rewrites is the cluster's catalogue directory, whoever's the source is.
// No pin moves, since the bytes a digest was taken over carry no origin.

// renameRequest is the new name.
type renameRequest struct {
	Name string `json:"name"`
}

func (s *Server) renameClusterCatalogue(w http.ResponseWriter, r *http.Request, c call) {
	s.renameCatalogue(w, r, c, "")
}

func (s *Server) renameTenantCatalogue(w http.ResponseWriter, r *http.Request, c call) {
	s.renameCatalogue(w, r, c, r.PathValue("t"))
}

func (s *Server) renameCatalogue(w http.ResponseWriter, r *http.Request, c call, tenant string) {
	var body renameRequest
	if err := decode(r, &body); err != nil || !gitops.ValidName(body.Name) {
		s.fail(w, r, http.StatusBadRequest, `body must be {"name": "<new name>"}: lower-case letters, digits and hyphens`)
		return
	}
	old := r.PathValue("s")
	res, err := s.cfg.Repo.RenameCatalogueSource(r.Context(), tenant, old, body.Name, c.meta)
	if err != nil {
		s.catalogueError(w, r, old, err)
		return
	}
	scope := scopeCluster
	if tenant != "" {
		scope = scopeTenant
	}
	answer := map[string]any{"status": res.Status, "name": old, "newName": body.Name, "scope": scope}
	if tenant != "" {
		answer["tenant"] = tenant
	}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}

// delegateCatalogues and undelegateCatalogues are the switch: whether a
// tenant's own administrators may add catalogues. The cluster
// administrator's alone, which is why there is no such route under
// /v1/tenants.
func (s *Server) delegateCatalogues(w http.ResponseWriter, r *http.Request, c call) {
	if s.refusedInPlatformTenant(w, r, r.PathValue("t")) {
		return
	}
	s.setDelegation(w, r, c, true)
}

func (s *Server) undelegateCatalogues(w http.ResponseWriter, r *http.Request, c call) {
	s.setDelegation(w, r, c, false)
}

func (s *Server) setDelegation(w http.ResponseWriter, r *http.Request, c call, delegated bool) {
	tenant := r.PathValue("t")
	res, err := s.cfg.Repo.SetTenantCatalogueDelegation(r.Context(), tenant, delegated, c.meta)
	if err != nil {
		s.catalogueError(w, r, "", err)
		return
	}
	answer := map[string]any{"status": res.Status, "tenant": tenant, "delegated": delegated}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}

func (s *Server) catalogueWritten(w http.ResponseWriter, r *http.Request, res gitops.Result, err error, scope, tenant string) {
	name := r.PathValue("s")
	if err != nil {
		s.catalogueError(w, r, name, err)
		return
	}
	answer := map[string]any{"status": res.Status, "name": name, "scope": scope}
	if tenant != "" {
		answer["tenant"] = tenant
	}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}

func (s *Server) catalogueError(w http.ResponseWriter, r *http.Request, name string, err error) {
	switch {
	case errors.Is(err, catalogue.ErrAddressRefused):
		s.fail(w, r, http.StatusUnprocessableEntity, "this address is not one a catalogue is fetched from: "+
			strings.TrimPrefix(err.Error(), catalogue.ErrAddressRefused.Error()+": ")+
			". A catalogue is a public https address; nothing was added")
	case errors.Is(err, catalogue.ErrArtifactRefused):
		s.fail(w, r, http.StatusUnprocessableEntity, "this artifact is not one a catalogue is pulled from: "+
			strings.TrimPrefix(err.Error(), catalogue.ErrArtifactRefused.Error()+": ")+
			". A catalogue artifact is <registry host>/<repository>:<tag> or @sha256:<digest>; nothing was added")
	case errors.Is(err, gitops.ErrRepositoryNotFound):
		// Said as "no such repository" whoever else may have the name: that a
		// name is another tenant's, or the cluster's, is not this caller's to
		// learn.
		s.fail(w, r, http.StatusUnprocessableEntity, "the catalogue names a repository that is not declared for "+ownerOf(r)+
			"; declare it first (kubectl gentian repositories declare <name> --url oci://<registry>/<path>"+tenantFlag(r)+"). Nothing was added")
	case errors.Is(err, gitops.ErrRepositoryNotRegistry), errors.Is(err, catalogue.ErrRegistryRefused):
		s.fail(w, r, http.StatusUnprocessableEntity, "the catalogue cannot be read with this repository: "+firstLine(err)+". Nothing was added")
	case errors.Is(err, gitops.ErrCatalogueNameTaken):
		s.fail(w, r, http.StatusConflict, strings.TrimPrefix(firstLine(err), gitops.ErrCatalogueNameTaken.Error()+": "))
	case errors.Is(err, gitops.ErrCatalogueNotFound):
		s.fail(w, r, http.StatusNotFound, "no such catalogue: "+name)
	case errors.Is(err, gitops.ErrCatalogueNotDelegated):
		s.fail(w, r, http.StatusForbidden, "adding and removing catalogues is not delegated to this tenant's administrators; "+
			"the cluster's administrator adds one for the tenant, or delegates it "+
			"(kubectl gentian tenants delegate-catalogues <tenant> on)")
	case errors.Is(err, gitops.ErrCatalogueNotTenants):
		s.fail(w, r, http.StatusForbidden, fmt.Sprintf(
			"%s was added for this tenant by the cluster's administrator, and only the cluster's administrator removes it", name))
	case errors.Is(err, gitops.ErrCatalogueFlowForm):
		s.fail(w, r, http.StatusConflict, gitops.ErrCatalogueFlowForm.Error())
	default:
		s.repoError(w, r, err)
	}
}

// ownerOf and tenantFlag say whose repositories a route's catalogue may be
// read with, for a message.
func ownerOf(r *http.Request) string {
	if t := r.PathValue("t"); t != "" {
		return "tenant " + t
	}
	return "the cluster"
}

func tenantFlag(r *http.Request) string {
	if t := r.PathValue("t"); t != "" {
		return " --tenant " + t
	}
	return ""
}

// firstLine is an error's message without the file it was found in, which
// the gitops layer appends for a log and a caller has no use for.
func firstLine(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, " in /"); i > 0 {
		msg = msg[:i]
	}
	return msg
}
