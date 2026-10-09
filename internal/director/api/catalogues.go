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
// would pretend to be a shop. Only ce and pe are listed: me and ee exist
// because somebody maintains or licenses them, and an entry whose whole value
// is a relationship with a supplier is not something a cluster can describe
// usefully, so it is counted and named to the store instead.
//
// None of this is a licence check. A source named on the Cluster claim is
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
// And it is read from one of two places: an https address, or a directory of
// the cluster's own deployments repository (gitops/catalogue_directory.go).
// The second is the cluster administrator's to declare, for the whole
// cluster or for one tenant, and never a tenant's administrator's: the
// repository is not theirs. It changes where the bytes come from and nothing
// else -- a catalogue kept there is a catalogue of the cluster or of a
// tenant exactly as one at an address is.
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
	// URL is the address it is served from; empty for a catalogue kept in
	// the deployments repository.
	URL string `json:"url"`
	// Path is the directory of the cluster's own deployments repository it
	// is kept in, for a catalogue that has no address.
	Path string `json:"path,omitempty"`
	// Scope is "cluster" for a catalogue every tenant sees and "tenant" for
	// one only this tenant does.
	Scope string `json:"scope"`
	// AddedBy is who added it: "cluster" for the cluster's administrator,
	// "tenant" for the tenant's own.
	AddedBy string `json:"addedBy"`
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
	// Installable says this entry can be installed from this list: it states
	// its digest, which is what an install from a source is pinned to. False
	// sends the person to the store.
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
		out = append(out, visibleSource{
			Source: catalogue.Source{Key: profilebundle.ClusterOrigin(src.Name), Name: src.Name, URL: src.URL, Dir: src.Path},
			scope:  scopeCluster, addedBy: string(gitops.ByCluster),
		})
	}
	for _, src := range own.Sources {
		if twice[src.Name] {
			continue
		}
		out = append(out, visibleSource{
			Source: catalogue.Source{Key: profilebundle.TenantOrigin(tenant, src.Name), Name: src.Name, URL: src.URL, Dir: src.Path},
			scope:  scopeTenant, addedBy: src.AddedBy,
		})
	}
	return out, own.Delegated, nil
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
		out = append(out, catalogueOut{Name: src.Name, URL: src.URL, Path: src.Dir, Scope: src.scope, AddedBy: src.addedBy})
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
		own = append(own, catalogueOut{Name: src.Name, URL: src.URL, Path: src.Path, Scope: scopeCluster, AddedBy: string(gitops.ByCluster)})
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
			entry.Catalogues = append(entry.Catalogues, catalogueOut{Name: src.Name, URL: src.URL, Path: src.Path, Scope: scopeTenant, AddedBy: src.AddedBy})
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
	case errors.Is(err, gitops.ErrCatalogueDirectoryRefused):
		s.cfg.Log.ErrorContext(r.Context(), "a catalogue's directory is not one a catalogue is read from",
			"request_id", reqID(r.Context()), "catalogue", src.Key, "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the directory of catalogue "+src.Name+
			" in the deployments repository is not one a catalogue is read from: "+refusedDirectory(err))
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
		entries = append(entries, entryOut{
			Coordinate: name + "/" + e.Name,
			Name:       e.Name,
			Version:    e.Version,
			Edition:    string(e.Edition),
			TrustTier:  e.TrustTier,
			Digest:     e.Digest,
			// Without a digest there is nothing to pin an install to, and
			// the store is the route.
			Installable: e.Digest != "",
		})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "catalogue": name, "scope": src.scope,
		"storeUrl": s.cfg.StoreURL, "entries": entries, "storeOnly": listing.StoreOnly,
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

// catalogueRequest says where the catalogue is: at an https address, or in a
// directory of the cluster's own deployments repository. One of the two.
type catalogueRequest struct {
	URL  string `json:"url"`
	Path string `json:"path"`
}

// vetted reads where a request says the catalogue is and checks it.
//
// An address is checked as it is written and by what its host resolves to,
// whoever sends it -- the cluster's administrator's typo must not make this
// process probe the cluster either. A directory is checked as it is written;
// that it is there is asked of the repository when it is added.
//
// mayNameDirectory is false on the route a tenant's own administrator
// reaches. The deployments repository is the cluster administrator's, and
// what is read from it is not a tenant's to say, delegated or not.
func (s *Server) vetted(w http.ResponseWriter, r *http.Request, mayNameDirectory bool) (catalogueRequest, bool) {
	var body catalogueRequest
	if err := decode(r, &body); err != nil || (body.URL == "") == (body.Path == "") {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"url": "https://<host>/<path>"}, or {"path": "<directory of the deployments repository>"}`)
		return catalogueRequest{}, false
	}
	if s.cfg.Catalogue == nil || s.cfg.Catalogue.Vet == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "this director fetches from no catalogue, so none can be added")
		return catalogueRequest{}, false
	}
	if body.Path != "" {
		if !mayNameDirectory {
			s.fail(w, r, http.StatusForbidden, "a catalogue kept in the cluster's deployments repository is added by the cluster's administrator, "+
				"for the cluster or for a tenant; a tenant's administrators add catalogues at an https address")
			return catalogueRequest{}, false
		}
		if s.cfg.Catalogue.Repo == nil {
			s.fail(w, r, http.StatusServiceUnavailable, "this director reads no catalogue from its deployments repository, so none can be added")
			return catalogueRequest{}, false
		}
		dir := strings.TrimSuffix(body.Path, "/")
		if err := gitops.CheckCatalogueDirectory(dir); err != nil {
			s.catalogueError(w, r, r.PathValue("s"), err)
			return catalogueRequest{}, false
		}
		return catalogueRequest{Path: dir}, true
	}
	if err := s.cfg.Catalogue.Vet(r.Context(), body.URL); err != nil {
		s.catalogueError(w, r, r.PathValue("s"), err)
		return catalogueRequest{}, false
	}
	return catalogueRequest{URL: strings.TrimSuffix(body.URL, "/")}, true
}

func (s *Server) addClusterCatalogue(w http.ResponseWriter, r *http.Request, c call) {
	where, ok := s.vetted(w, r, true)
	if !ok {
		return
	}
	var res gitops.Result
	var err error
	if where.Path != "" {
		res, err = s.cfg.Repo.AddClusterCatalogueDirectory(r.Context(), r.PathValue("s"), where.Path, c.meta)
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
	where, ok := s.vetted(w, r, by == gitops.ByCluster)
	if !ok {
		return
	}
	var res gitops.Result
	var err error
	if where.Path != "" {
		// Reached only as the cluster's administrator (vetted), and the
		// method takes no role to get wrong.
		res, err = s.cfg.Repo.AddTenantCatalogueDirectory(r.Context(), tenant, r.PathValue("s"), where.Path, c.meta)
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
	case errors.Is(err, gitops.ErrCatalogueDirectoryRefused):
		s.fail(w, r, http.StatusUnprocessableEntity, "this directory is not one a catalogue is read from: "+refusedDirectory(err)+
			". A catalogue kept in the deployments repository is a directory of it holding index.yaml and profiles/; nothing was added")
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

// refusedDirectory is why a directory was refused, without the words every
// such refusal begins with.
func refusedDirectory(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, gitops.ErrCatalogueDirectoryRefused.Error()+": "); i >= 0 {
		msg = msg[i+len(gitops.ErrCatalogueDirectoryRefused.Error())+2:]
	}
	return msg
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
