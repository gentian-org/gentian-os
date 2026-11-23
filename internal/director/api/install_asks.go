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
	"net/http"
	"slices"
	"strings"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/hostnames"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
	"github.com/gentian-org/gentian-os/internal/security"
)

// What a build asks for, said before it is installed, and its answer given
// with the install.
//
// An app that asks for a way out of the tenant is held until the tenant's
// perimeter approver answers (privileges.go). Whoever installs the app and
// holds that right can answer in the install itself: the director says what
// the build and the apps it comes with ask for, the install names the
// requests approved, and each is recorded as a grant by that person, exactly
// as an approval made afterwards is. Egress alone: the other kinds of
// privilege are answered at the cluster's scope, by somebody who is not the
// tenant's administrator.

// askedEgress is one egress request of a build about to be installed.
type askedEgress struct {
	// Install is the app the request is of: the app itself, or one it comes
	// with.
	Install string `json:"install"`
	// Privilege is egress/<name>, as an approval names it.
	Privilege string `json:"privilege"`
	// Reason is what the profile said it needs it for.
	Reason string `json:"reason,omitempty"`
	// Hosts are the hosts a request by host names; none for one by address.
	Hosts []string `json:"hosts,omitempty"`
	// HostsAtApproval says the approver names the hosts, for an app whose
	// destinations its installation chooses.
	HostsAtApproval bool `json:"hostsAtApproval,omitempty"`
	// CameWith names the app this one comes with, for a request that is not
	// the app's own.
	CameWith string `json:"cameWith,omitempty"`

	// answered says the request waits for nobody: policy answers it, or it
	// was granted on this install before.
	answered bool
}

// askedIntegration is one contract a build about to be installed consumes
// from another app of the tenant: what the app reaches once the tenant's
// administrator grants it.
type askedIntegration struct {
	// Install is the app that consumes: the app itself, or one it comes with.
	Install string `json:"install"`
	// Contract is the contract consumed, as a grant names it.
	Contract string `json:"contract"`
	// Provider is the app the profile expects to provide it; empty where the
	// tenant's binding chooses.
	Provider string `json:"provider,omitempty"`
	// Capabilities are what of the contract the build asks for, and what a
	// grant with the install grants: no more, no less.
	Capabilities []string `json:"capabilities"`
	// AsPerson says the app calls the provider as the person it acts for.
	AsPerson bool `json:"asPerson,omitempty"`
	// CameWith names the app this one comes with, for a contract that is not
	// the app's own.
	CameWith string `json:"cameWith,omitempty"`

	// answered says the contract waits for nobody: the app's grant names it
	// already, or the platform pre-approves it among apps installed together.
	answered bool
}

// grantedContract is one contract an install names as granted.
type grantedContract struct {
	Install  string `json:"install"`
	Contract string `json:"contract"`
}

// buildAsks is everything a build about to be installed asks for that
// somebody of the tenant answers.
type buildAsks struct {
	egress       []askedEgress
	integrations []askedIntegration
}

// approvedRequest is one request an install names as approved.
type approvedRequest struct {
	Install   string `json:"install"`
	Privilege string `json:"privilege"`
}

// egressAnswers is what already answers egress requests in a tenant.
type egressAnswers struct {
	domains []string
	grants  []gitops.PrivilegeGrant
}

// egressAnswersFor reads them: the cluster's policy, the tenant's own
// trusted domains while it was delegated a list, and the tenant's grants.
func (s *Server) egressAnswersFor(ctx context.Context, tenant string) (egressAnswers, error) {
	domains, err := s.cfg.Repo.EgressPolicy(ctx)
	if err != nil {
		return egressAnswers{}, err
	}
	own, err := s.cfg.Repo.TenantTrustedDomains(ctx, tenant)
	if err != nil && !errors.Is(err, gitops.ErrTenantNotFound) {
		return egressAnswers{}, err
	}
	if own.Delegated {
		domains = append(append([]string(nil), domains...), own.Domains...)
	}
	grants, err := s.cfg.Repo.TenantPrivileges(ctx, tenant)
	if err != nil && !errors.Is(err, gitops.ErrTenantNotFound) {
		return egressAnswers{}, err
	}
	return egressAnswers{domains: domains, grants: grants}, nil
}

// asksOf are the egress requests one profile states, installed as install
// from origin, each with whether it waits for nobody. A profile no catalog
// brought is the platform's own, whose requests the platform answers.
func asksOf(install string, definition *gentianov1alpha1.ComponentProfile, origin, cameWith string, have egressAnswers) []askedEgress {
	if definition == nil || definition.Privileges() == nil || len(definition.Privileges().Egress) == 0 {
		return nil
	}
	read := definition.DeepCopy()
	if origin != "" {
		if read.Annotations == nil {
			read.Annotations = map[string]string{}
		}
		read.Annotations[profilebundle.OriginAnnotation] = origin
	}
	shipped := hostnames.PlatformShipped(read)
	byDomain := security.TrustedEgress(read, have.domains)
	now := time.Now()
	var out []askedEgress
	for i := range read.Privileges().Egress {
		e := &read.Privileges().Egress[i]
		ref := security.PrivilegeRef(security.PrivilegeEgress, e.Name)
		_, domain := byDomain[ref]
		granted := slices.ContainsFunc(have.grants, func(g gitops.PrivilegeGrant) bool {
			if g.Install != install || g.Privilege != ref {
				return false
			}
			if g.ExpiresAt == "" {
				return true
			}
			until, err := time.Parse(time.RFC3339, g.ExpiresAt)
			return err == nil && until.After(now)
		})
		out = append(out, askedEgress{
			Install: install, Privilege: ref, Reason: e.Reason, Hosts: e.Hosts, HostsAtApproval: e.HostsAtApproval, CameWith: cameWith,
			answered: (shipped && !e.HostsAtApproval) || domain || granted,
		})
	}
	return out
}

// integrationsOf are the contracts one profile consumes, installed as
// install, each with whether it waits for nobody. A contract that names no
// capability is left out: there is nothing to grant of it with the install.
func integrationsOf(install string, definition *gentianov1alpha1.ComponentProfile, cameWith string,
	granted []gentianov1alpha1.TenantAppGrant, together map[string]bool,
) []askedIntegration {
	if definition == nil {
		return nil
	}
	var out []askedIntegration
	for _, in := range definition.Spec.Integrations {
		if in.Contract == "" || len(in.Capabilities) == 0 || in.Provider == install {
			continue
		}
		named := slices.ContainsFunc(granted, func(g gentianov1alpha1.TenantAppGrant) bool {
			return g.App == install && slices.ContainsFunc(g.Consume, func(c gentianov1alpha1.ConsumeGrantSpec) bool {
				return c.Contract == in.Contract
			})
		})
		out = append(out, askedIntegration{
			Install: install, Contract: in.Contract, Provider: in.Provider, Capabilities: in.Capabilities,
			AsPerson: in.AsPerson, CameWith: cameWith,
			answered: named || (in.Provider != "" && together[in.Provider]),
		})
	}
	return out
}

// installAsks are the egress requests and the integrations an install of
// profile at definition would bring to tenant: the app's own, and those of
// the apps it comes with that the tenant does not have. An app it comes with
// whose build cannot be read is left out: what it asks for waits for the
// tenant as it always did.
func (s *Server) installAsks(
	ctx context.Context, tenant, profile string, definition *gentianov1alpha1.ComponentProfile, origin, preferred string,
) (buildAsks, error) {
	if definition == nil {
		return buildAsks{}, nil
	}
	have, err := s.egressAnswersFor(ctx, tenant)
	if err != nil {
		return buildAsks{}, err
	}
	granted, err := s.cfg.Repo.TenantAppGrants(ctx, tenant)
	if err != nil && !errors.Is(err, gitops.ErrTenantNotFound) {
		return buildAsks{}, err
	}
	if origin == "" {
		origin = definition.Annotations[profilebundle.OriginAnnotation]
	}
	// The apps this install brings, which the platform pre-approves each
	// other's contracts among (approveAmong).
	type brought struct {
		name       string
		definition *gentianov1alpha1.ComponentProfile
		origin     string
	}
	bringing := []brought{{profile, definition, origin}}
	if len(definition.Spec.ComesWith) > 0 {
		apps, err := s.cfg.Repo.Apps(ctx, tenant)
		if err != nil && !errors.Is(err, gitops.ErrTenantNotFound) {
			return buildAsks{}, err
		}
		for _, name := range definition.Spec.ComesWith {
			if name == profile || gitops.BroughtPlatformProfile(name) ||
				slices.ContainsFunc(apps, func(a gitops.App) bool { return a.Profile == name }) {
				continue
			}
			found, why, err := s.findCompanion(ctx, tenant, name, preferred)
			if err != nil {
				return buildAsks{}, err
			}
			if why != "" {
				continue
			}
			with, from := s.companionDefinition(ctx, tenant, found)
			bringing = append(bringing, brought{name, with, from})
		}
	}
	together := map[string]bool{}
	if len(bringing) > 1 {
		for _, b := range bringing {
			together[b.name] = true
		}
	}
	var out buildAsks
	for _, b := range bringing {
		cameWith := ""
		if b.name != profile {
			cameWith = profile
		}
		out.egress = append(out.egress, asksOf(b.name, b.definition, b.origin, cameWith, have)...)
		out.integrations = append(out.integrations, integrationsOf(b.name, b.definition, cameWith, granted, together)...)
	}
	return out, nil
}

// companionDefinition reads the profile of an app another comes with and
// the origin it is recorded with: the build its catalog offers, or the
// profile the cluster holds. Nil where it cannot be read. It writes nothing.
func (s *Server) companionDefinition(ctx context.Context, tenant string, c companion) (*gentianov1alpha1.ComponentProfile, string) {
	if c.catalogue == "" {
		definition, err := s.cfg.Repo.ProfileDefinition(ctx, c.name)
		if err != nil || definition == nil {
			return nil, ""
		}
		return definition, definition.Annotations[profilebundle.OriginAnnotation]
	}
	source, known, err := s.resolveSource(ctx, tenant, c.catalogue)
	if err != nil || !known {
		return nil, ""
	}
	if stored, found, err := s.cfg.Repo.BuildOnCluster(ctx, c.name, c.digest); err == nil && found && stored.Origin == source.origin(tenant) {
		if definition, err := profilebundle.ReadProfile(stored.Body); err == nil {
			return definition, source.origin(tenant)
		}
	}
	if s.cfg.Catalogue == nil {
		return nil, ""
	}
	fetched, err := s.cfg.Catalogue.Fetch(ctx, source.Source, c.name, c.digest)
	if err != nil || fetched == nil {
		return nil, ""
	}
	return fetched.Definition, source.origin(tenant)
}

// catalogueEntryAsks answers what one build of a catalog entry asks for,
// before it is installed: the egress requests that would wait for the
// tenant's perimeter approver and the integrations that would wait for a
// grant, and whether the caller may give each.
//
// Under can_view, like the listing the build was chosen from. The build is
// fetched and held to its digest as an install holds it; nothing is written.
func (s *Server) catalogueEntryAsks(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	tenant, sourceName, profile := r.PathValue("t"), r.PathValue("s"), r.PathValue("p")
	digest, err := catalogue.CanonicalDigest(r.URL.Query().Get("digest"))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "digest must be sha256:<64 hex characters>")
		return
	}
	source, entry, ok := s.pinOrigin(w, r, tenant, sourceName+"/"+profile, digest)
	if !ok {
		return
	}
	if source == nil || entry != profile {
		s.fail(w, r, http.StatusBadRequest, "the path must name a catalog and one of its entries")
		return
	}
	fetched, ok := s.fetchOrStored(w, r, *source, tenant, entry, digest)
	if !ok {
		return
	}
	asks, err := s.installAsks(ctx, tenant, profile, fetched.Definition, source.origin(tenant), source.Name)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	waiting := []askedEgress{}
	for _, a := range asks.egress {
		if !a.answered {
			waiting = append(waiting, a)
		}
	}
	ungranted := []askedIntegration{}
	for _, a := range asks.integrations {
		if !a.answered {
			ungranted = append(ungranted, a)
		}
	}
	// Each asked only where there is something to answer: a tenant that
	// cannot be asked about leaves the choice out, and the install works as
	// it does without one.
	mayApprove, mayGrant := false, false
	if target, err := tenantObject(r); err == nil {
		if len(waiting) > 0 {
			mayApprove, _ = s.can(ctx, c, "can_approve_privilege", target)
		}
		if len(ungranted) > 0 {
			mayGrant, _ = s.can(ctx, c, "can_grant", target)
		}
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "profile": profile, "digest": digest,
		"egress": waiting, "mayApprove": mayApprove,
		"integrations": ungranted, "mayGrant": mayGrant,
	})
}

// mayApproveWithInstall settles, before anything is fetched or written,
// that the requests an install names as approved are ones this caller may
// approve: egress, which the tenant's perimeter approver answers. It writes
// the refusal itself and reports false. The decision it returns is what the
// grants' commits record.
func (s *Server) mayApproveWithInstall(w http.ResponseWriter, r *http.Request, c call, tenant string, approve []approvedRequest) (string, bool) {
	if len(approve) == 0 {
		return "", true
	}
	for _, a := range approve {
		kind, name := security.SplitPrivilegeRef(a.Privilege)
		if !gitops.ValidName(a.Install) || kind != security.PrivilegeEgress || name == "" {
			s.fail(w, r, http.StatusBadRequest,
				`approve names egress requests: [{"install": "<app>", "privilege": "egress/<name>"}]; `+
					"every other kind of privilege is approved at the cluster's scope")
			return "", false
		}
	}
	target := authz.Tenant(tenant)
	ok, err := s.can(r.Context(), c, "can_approve_privilege", target)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return "", false
	}
	if !ok {
		s.fail(w, r, http.StatusForbidden,
			"approving an egress request is the perimeter approver's; install the app without approve, "+
				"and it waits for their answer. Nothing was installed")
		return "", false
	}
	return "can_approve_privilege " + target, true
}

// notAsked names the first request an install approves that the build and
// the apps it comes with do not state, or "".
func notAsked(approve []approvedRequest, asks []askedEgress) string {
	for _, a := range approve {
		if !slices.ContainsFunc(asks, func(e askedEgress) bool { return e.Install == a.Install && e.Privilege == a.Privilege }) {
			return a.Install + " " + a.Privilege
		}
	}
	return ""
}

// grantWithInstall records the approvals an install named, once the install
// is written: one grant each, by the caller, for every request that still
// waits. One that policy answers, or that was granted before, is left as it
// is, and so is one whose hosts are named at approval. A grant that cannot be written leaves its request waiting, as if it
// had not been named; the answer lists what was approved.
func (s *Server) grantWithInstall(ctx context.Context, c call, tenant, decision string, approve []approvedRequest, asks []askedEgress) []approvedRequest {
	approved := []approvedRequest{}
	meta := c.meta
	meta.Decision = decision
	for _, a := range approve {
		// A request whose hosts are named at approval is approved where the
		// hosts are named, not with the install.
		waits := slices.ContainsFunc(asks, func(e askedEgress) bool {
			return e.Install == a.Install && e.Privilege == a.Privilege && !e.answered && !e.HostsAtApproval
		})
		if !waits {
			continue
		}
		_, err := s.cfg.Repo.GrantPrivilege(ctx, tenant, gitops.PrivilegeGrant{
			Install:    a.Install,
			Privilege:  a.Privilege,
			Approver:   c.meta.Subject,
			ApprovedAt: time.Now().UTC().Format(time.RFC3339),
			Reason:     "approved with the install of the app",
		}, meta)
		if err != nil {
			s.cfg.Log.WarnContext(ctx, "an approval given with an install could not be written; the request waits",
				"tenant", tenant, "install", a.Install, "privilege", a.Privilege, "error", err.Error())
			continue
		}
		approved = append(approved, a)
	}
	return approved
}

// mayGrantWithInstall settles, before anything is fetched or written, that
// the caller may grant the integrations an install names: can_grant on the
// tenant, as on the Integrations screen. It writes the refusal itself and
// reports false. The decision it returns is what the grants' commits record.
func (s *Server) mayGrantWithInstall(w http.ResponseWriter, r *http.Request, c call, tenant string, grant []grantedContract) (string, bool) {
	if len(grant) == 0 {
		return "", true
	}
	for _, g := range grant {
		if !gitops.ValidName(g.Install) || !gitops.ValidName(g.Contract) {
			s.fail(w, r, http.StatusBadRequest, `grant names contracts: [{"install": "<app>", "contract": "<contract>"}]`)
			return "", false
		}
	}
	target := authz.Tenant(tenant)
	ok, err := s.can(r.Context(), c, "can_grant", target)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return "", false
	}
	if !ok {
		s.fail(w, r, http.StatusForbidden,
			"granting what an app may consume needs can_grant on the tenant; install the app without grant, "+
				"and its integrations wait for somebody who may grant. Nothing was installed")
		return "", false
	}
	return "can_grant " + target, true
}

// notConsumed names the first contract an install grants that the build and
// the apps it comes with do not consume, or "".
func notConsumed(grant []grantedContract, asks []askedIntegration) string {
	for _, g := range grant {
		if !slices.ContainsFunc(asks, func(a askedIntegration) bool { return a.Install == g.Install && a.Contract == g.Contract }) {
			return g.Install + " " + g.Contract
		}
	}
	return ""
}

// grantIntegrationsWithInstall records the grants an install named, once the
// install is written: each contract with exactly what the build asks of it,
// by the caller. A contract the app's grant already names is left as it
// stands, so a narrower grant is not widened. A grant that cannot be written
// leaves its contract waiting, as if it had not been named; the answer lists
// what was granted.
func (s *Server) grantIntegrationsWithInstall(ctx context.Context, c call, tenant, decision string, grant []grantedContract, asks []askedIntegration) []grantedContract {
	granted := []grantedContract{}
	meta := c.meta
	meta.Decision = decision
	for _, g := range grant {
		i := slices.IndexFunc(asks, func(a askedIntegration) bool {
			return a.Install == g.Install && a.Contract == g.Contract && !a.answered
		})
		if i < 0 {
			continue
		}
		_, err := s.cfg.Repo.GrantConsumeWithInstall(ctx, tenant, g.Install, g.Contract, asks[i].Capabilities, meta)
		if err != nil {
			s.cfg.Log.WarnContext(ctx, "a grant given with an install could not be written; the integration waits",
				"tenant", tenant, "install", g.Install, "contract", g.Contract, "error", err.Error())
			continue
		}
		granted = append(granted, g)
	}
	return granted
}

// grantSummary says an install's grants in a refusal.
func grantSummary(grant []grantedContract) string {
	parts := make([]string, 0, len(grant))
	for _, g := range grant {
		parts = append(parts, g.Install+" "+g.Contract)
	}
	return strings.Join(parts, ", ")
}

// approveSummary says an install's approvals in a refusal.
func approveSummary(approve []approvedRequest) string {
	parts := make([]string, 0, len(approve))
	for _, a := range approve {
		parts = append(parts, a.Install+" "+a.Privilege)
	}
	return strings.Join(parts, ", ")
}
