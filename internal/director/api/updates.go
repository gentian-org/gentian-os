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
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/platformversion"
)

// The report of outdated apps, and the moves it leads to
// (docs/plans/versioning.md §4.2, §5).
//
// Three things are read together and nothing is stored: what git says each
// tenant has installed and at which build; what the cluster runs and how
// the last move went (the operator's rollouts); and what the catalog each
// install came from offers now. An app is outdated when the catalog's
// newest build its floor admits is not the one pinned. What keeps it there is
// said beside it -- its mode, a customization that carries upgrade risk, a
// move that was rolled back -- so the person reading the report knows what
// one press of "move" would do and what it would not.

const (
	modeFromTenant        = "tenant"
	modeFromApp           = "app"
	modeFromCluster       = "cluster"
	modeFromCustomization = "customization"

	rolloutsPath       = "/v1/rollouts"
	customizationsPath = "/v1/customizations"
)

// updateEntry is one installed app in the report.
type updateEntry struct {
	Tenant  string `json:"tenant"`
	Profile string `json:"profile"`
	// AddonOf names the app an add-on's pin sits in: the entry is that
	// add-on, and it moves under its app's mode. The cluster runs an add-on
	// inside its app's release, so it has no build or rollout of its own.
	AddonOf string `json:"addonOf,omitempty"`
	// Pinned is the build git names; Running the build the cluster runs.
	Pinned  string `json:"pinned,omitempty"`
	Running string `json:"running,omitempty"`
	// Catalogue is the source the newest build is read from, and Newest the
	// newest build there that this platform admits, with its version.
	Catalogue     string `json:"catalogue,omitempty"`
	Newest        string `json:"newest,omitempty"`
	NewestVersion string `json:"newestVersion,omitempty"`
	// Outdated: Newest is known and is not Pinned.
	Outdated bool `json:"outdated"`
	// Unmet is why the catalog's newest build cannot be installed here.
	Unmet string `json:"unmet,omitempty"`
	// Mode is how this app moves for this tenant, and ModeFrom where that
	// answer came from: tenant, app, customization or cluster.
	Mode     string `json:"mode"`
	ModeFrom string `json:"modeFrom"`
	// Held says what keeps an outdated app where it is. Empty: the platform
	// moves it on the next latest run, or a person may.
	Held []string `json:"held,omitempty"`
	// Rollout is the move under way or the last one, as the cluster reports.
	Rollout *rolloutView `json:"rollout,omitempty"`
	// Ready is the instance's Ready condition.
	Ready bool `json:"ready"`
}

type rolloutView struct {
	Target  string `json:"target,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	MovedAt string `json:"movedAt,omitempty"`
	MovedBy string `json:"movedBy,omitempty"`
}

// updateReport is the answer.
type updateReport struct {
	Cluster string              `json:"cluster"`
	Policy  gitops.UpdatePolicy `json:"policy"`
	// WindowOpen is whether the platform would move latest-mode apps now.
	WindowOpen bool          `json:"windowOpen"`
	Entries    []updateEntry `json:"entries"`
	// Unread names catalogs whose index could not be read; entries from
	// them say nothing about a newer build.
	Unread []string `json:"unread,omitempty"`
	// Partial says the cluster's side could not be read, and why.
	Partial string `json:"partial,omitempty"`
}

// updatesForCluster answers the report for every tenant.
func (s *Server) updatesForCluster(w http.ResponseWriter, r *http.Request, _ call) {
	report, err := s.updateReport(r.Context(), "")
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, report)
}

// updatesForTenant answers the report for one tenant.
func (s *Server) updatesForTenant(w http.ResponseWriter, r *http.Request, _ call) {
	report, err := s.updateReport(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, report)
}

// updateReport computes the report; tenant "" is every tenant.
func (s *Server) updateReport(ctx context.Context, tenant string) (*updateReport, error) {
	policy, err := s.cfg.Repo.UpdatePolicy(ctx)
	if err != nil {
		return nil, err
	}
	report := &updateReport{Cluster: s.cfg.Cluster, Policy: policy, WindowOpen: policy.Window.Open(time.Now()), Entries: []updateEntry{}}
	tenants := []string{tenant}
	if tenant == "" {
		if tenants, err = s.cfg.Repo.Tenants(ctx); err != nil {
			return nil, err
		}
	}
	rollouts, holds, partial := s.clusterSide(ctx)
	report.Partial = partial
	unread := map[string]bool{}
	for _, t := range tenants {
		apps, err := s.cfg.Repo.Apps(ctx, t)
		if err != nil {
			return nil, err
		}
		sources, _, err := s.visibleSources(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, app := range apps {
			if app.Profile == "" || app.Digest == "" {
				// An install pinned to nothing follows the profile on the
				// cluster and has no build to be outdated against.
				continue
			}
			e := updateEntry{Tenant: t, Profile: app.Profile, Pinned: app.Digest}
			if ro, ok := rollouts[t+"/"+app.Profile]; ok {
				e.Running, e.Ready = ro.Running, ro.Ready
				if ro.Phase != "" {
					e.Rollout = &rolloutView{Target: ro.Target, Phase: ro.Phase, Reason: ro.Reason, Message: ro.Message, MovedAt: ro.MovedAt, MovedBy: ro.MovedBy}
				}
			}
			e.Mode, e.ModeFrom = s.effectiveMode(policy, app, holds[app.Profile], t)
			src, entry, ok := s.newestFor(ctx, sources, app, unread)
			if ok {
				e.Catalogue, e.Newest, e.NewestVersion = src.Name, entry.Digest, entry.Version
				e.Unmet = platformversion.Unmet(entry.Platform)
				e.Outdated = entry.Digest != "" && entry.Digest != app.Digest && e.Unmet == ""
			}
			e.Held = heldReasons(e, holds[app.Profile], t)
			report.Entries = append(report.Entries, e)
			for _, pin := range app.AddonPins {
				report.Entries = append(report.Entries, s.addonEntry(ctx, sources, e, pin, holds[app.Profile], unread))
			}
		}
	}
	for name := range unread {
		report.Unread = append(report.Unread, name)
	}
	sort.Strings(report.Unread)
	// By tenant and app, each app's add-ons right after it.
	app := func(e updateEntry) string {
		if e.AddonOf != "" {
			return e.AddonOf
		}
		return e.Profile
	}
	sort.SliceStable(report.Entries, func(i, j int) bool {
		a, b := report.Entries[i], report.Entries[j]
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		if app(a) != app(b) {
			return app(a) < app(b)
		}
		if (a.AddonOf == "") != (b.AddonOf == "") {
			return a.AddonOf == ""
		}
		return a.Profile < b.Profile
	})
	return report, nil
}

// addonEntry is one pinned add-on of an app in the report: compared with the
// catalogue its build came from, under its app's mode, and held by what
// holds its app -- and while its app's own move is under way.
func (s *Server) addonEntry(ctx context.Context, sources []visibleSource, base updateEntry, pin gitops.AddonPin, holds []customizationHold, unread map[string]bool) updateEntry {
	e := updateEntry{
		Tenant: base.Tenant, Profile: pin.Name, AddonOf: base.Profile, Pinned: pin.Digest,
		Mode: base.Mode, ModeFrom: base.ModeFrom, Ready: base.Ready,
	}
	src, entry, ok := s.newestFor(ctx, sources, gitops.App{Profile: pin.Name, Catalogue: pin.Catalogue}, unread)
	if ok {
		e.Catalogue, e.Newest, e.NewestVersion = src.Name, entry.Digest, entry.Version
		e.Unmet = platformversion.Unmet(entry.Platform)
		e.Outdated = entry.Digest != "" && entry.Digest != pin.Digest && e.Unmet == ""
	}
	e.Held = heldReasons(e, holds, base.Tenant)
	if e.Outdated && base.Rollout != nil && (base.Rollout.Phase == "Exporting" || base.Rollout.Phase == "Rolling") {
		e.Held = append(e.Held, "the move of "+base.Profile+" is under way")
	}
	return e
}

// operatorRollout is one entry of the operator's rollouts.
type operatorRollout struct {
	Tenant, Profile, Pinned, Running, Target, Phase, Reason, Message, MovedAt, MovedBy string
	Ready                                                                              bool
}

// customizationHold is a customization record that keeps an app on a manual
// move: at L3 or above, carrying upgrade risk.
type customizationHold struct {
	Name, Rung, Risk string
	Tenants          []string
}

func (h customizationHold) applies(tenant string) bool {
	if len(h.Tenants) == 0 {
		return true
	}
	for _, t := range h.Tenants {
		if t == tenant {
			return true
		}
	}
	return false
}

// clusterSide reads the rollouts and the customizations from the operator.
// Either not being readable leaves the report partial rather than empty:
// what git and the catalog say still stands.
func (s *Server) clusterSide(ctx context.Context) (map[string]operatorRollout, map[string][]customizationHold, string) {
	rollouts := map[string]operatorRollout{}
	holds := map[string][]customizationHold{}
	if s.cfg.Lifecycle == nil {
		// A director with no operator to ask: git and the catalog are
		// still read, and the report says the cluster's side is missing.
		return rollouts, holds, "this director has no operator to ask for the cluster's rollouts and customizations"
	}
	var partial []string
	if status, body, err := s.cfg.Lifecycle.Get(ctx, rolloutsPath, nil); err != nil || status != http.StatusOK {
		partial = append(partial, "the cluster's rollouts could not be read: "+upstreamFailure(status, body, err))
	} else {
		var answer struct {
			Rollouts []operatorRollout `json:"rollouts"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			partial = append(partial, "the cluster's rollouts could not be read: "+err.Error())
		}
		for _, ro := range answer.Rollouts {
			rollouts[ro.Tenant+"/"+ro.Profile] = ro
		}
	}
	if status, body, err := s.cfg.Lifecycle.Get(ctx, customizationsPath, nil); err != nil || status != http.StatusOK {
		partial = append(partial, "the cluster's customizations could not be read: "+upstreamFailure(status, body, err))
	} else {
		var answer struct {
			Records []struct {
				Name, TargetProfile, Rung, UpgradeRisk string
				Tenants                                []string
			} `json:"records"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			partial = append(partial, "the cluster's customizations could not be read: "+err.Error())
		}
		for _, rec := range answer.Records {
			if rec.UpgradeRisk == "" || !rungAtOrAbove(rec.Rung, 3) {
				continue
			}
			holds[rec.TargetProfile] = append(holds[rec.TargetProfile], customizationHold{Name: rec.Name, Rung: rec.Rung, Risk: rec.UpgradeRisk, Tenants: rec.Tenants})
		}
	}
	return rollouts, holds, strings.Join(partial, "; ")
}

func upstreamFailure(status int, body []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("%d %s", status, lifecycle.ErrorMessage(body))
}

// rungAtOrAbove reads "L3" and the like.
func rungAtOrAbove(rung string, n int) bool {
	var level int
	if _, err := fmt.Sscanf(strings.ToUpper(rung), "L%d", &level); err != nil {
		return false
	}
	return level >= n
}

// effectiveMode is how an app moves for a tenant, and where the answer came
// from: the tenant's override, the cluster's answer for the app, a
// customization carrying upgrade risk (manual), the cluster's mode.
func (s *Server) effectiveMode(policy gitops.UpdatePolicy, app gitops.App, holds []customizationHold, tenant string) (string, string) {
	if gitops.ValidUpdateMode(app.UpdateMode) {
		return app.UpdateMode, modeFromTenant
	}
	if mode, ok := policy.Apps[app.Profile]; ok && gitops.ValidUpdateMode(string(mode)) {
		return string(mode), modeFromApp
	}
	for _, h := range holds {
		if h.applies(tenant) {
			return string(gitops.UpdateManual), modeFromCustomization
		}
	}
	return string(policy.ModeFor(app.Profile, "")), modeFromCluster
}

// heldReasons says what keeps an outdated app where it is.
func heldReasons(e updateEntry, holds []customizationHold, tenant string) []string {
	if !e.Outdated {
		return nil
	}
	var out []string
	switch e.Mode {
	case string(gitops.UpdateManual):
		if e.ModeFrom == modeFromCustomization {
			for _, h := range holds {
				if h.applies(tenant) {
					out = append(out, fmt.Sprintf("customization %s at %s carries upgrade risk %s: moved by hand", h.Name, h.Rung, h.Risk))
				}
			}
		} else {
			out = append(out, "mode manual: the cluster's administrator moves it")
		}
	case string(gitops.UpdateTenant):
		out = append(out, "mode tenant: the tenant's administrator moves it")
	}
	if e.Rollout != nil && e.Rollout.Target == e.Newest {
		switch e.Rollout.Phase {
		case "RolledBack":
			out = append(out, "the move to this build was rolled back: "+e.Rollout.Message)
		case "Failed":
			out = append(out, "the move to this build did not start: "+e.Rollout.Message)
		case "Exporting", "Rolling":
			out = append(out, "the move to this build is under way")
		}
	}
	return out
}

// newestFor is the catalog entry an install is compared with: from the
// catalogue it was installed from where the tenant still sees it, otherwise
// from the first catalogue the tenant sees that serves the name.
//
// The entries a listing leaves to the store count here. An app is acquired
// once, not per build: a tenant that has a store-only app installed moves
// to its newest build like any other tenant does, whether the platform
// moves it or a person does. A new install of one is still the store's.
func (s *Server) newestFor(ctx context.Context, sources []visibleSource, app gitops.App, unread map[string]bool) (visibleSource, catalogue.IndexEntry, bool) {
	if s.cfg.Catalogue == nil {
		return visibleSource{}, catalogue.IndexEntry{}, false
	}
	ordered := make([]visibleSource, 0, len(sources))
	for _, src := range sources {
		if src.Name == app.Catalogue {
			ordered = append([]visibleSource{src}, ordered...)
		} else {
			ordered = append(ordered, src)
		}
	}
	for _, src := range ordered {
		listing, err := s.cfg.Catalogue.Index(ctx, src.Source)
		if err != nil {
			unread[src.Name] = true
			continue
		}
		for _, e := range append(append(listing.Entries, listing.StoreOnly...), listing.Placed...) {
			if e.Name == app.Profile {
				return src, e, true
			}
		}
	}
	return visibleSource{}, catalogue.IndexEntry{}, false
}

// endedBadly is the last move of a tenant's app to digest when the operator
// reports it rolled back or not started, and nil otherwise: a move under way,
// one that is done, one to another build, or a cluster that cannot be asked.
func (s *Server) endedBadly(ctx context.Context, tenant, profile, digest string) *rolloutView {
	rollouts, _, _ := s.clusterSide(ctx)
	ro, ok := rollouts[tenant+"/"+profile]
	if !ok || ro.Target != digest || (ro.Phase != "RolledBack" && ro.Phase != "Failed") {
		return nil
	}
	return &rolloutView{Target: ro.Target, Phase: ro.Phase, Reason: ro.Reason, Message: ro.Message, MovedAt: ro.MovedAt, MovedBy: ro.MovedBy}
}

// retryMessage says which move is being asked for again, and why it ended.
func retryMessage(ro *rolloutView) string {
	why := ro.Message
	if why == "" {
		why = ro.Reason
	}
	if ro.Phase == "Failed" {
		return "retrying the move that did not start: " + why
	}
	return "retrying the move that was rolled back: " + why
}

// retryMove answers an install of the build an app is pinned to, whose move
// to it ended badly: the entry records a fresh move, and the operator starts
// it again (internal/controller/component_move.go).
func (s *Server) retryMove(w http.ResponseWriter, r *http.Request, c call, tenant, profile, digest string, ro *rolloutView) {
	res, err := s.cfg.Repo.RetryMove(r.Context(), tenant, profile, digest, c.meta)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	answer := map[string]any{"status": res.Status, "message": retryMessage(ro)}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}

// retryable is an entry whose last move, to the build it is pinned to, was
// rolled back or did not start: current by the catalog, and not running
// what it is pinned to.
func retryable(e updateEntry) bool {
	return !e.Outdated && e.Pinned != "" && e.Rollout != nil && e.Rollout.Target == e.Pinned &&
		(e.Rollout.Phase == "RolledBack" || e.Rollout.Phase == "Failed")
}

// moveOutdated moves every outdated app the body names -- all of them when
// it names none -- to the newest build its catalog offers, by the one
// operation an install is (install), under the cluster administrator's
// right. Apps held by their mode are moved too: this is the administrator
// acting, which every mode allows. An app whose newest build this platform
// does not admit, or whose move to that build is under way, is skipped and
// said. An app whose last move, to the build it is pinned to, was rolled back
// or did not start is asked to move once more, and reported as "retried".
// Only here and by an install of that build: the platform's own mover never
// retries, so a build that keeps failing is not tried again and again.
func (s *Server) moveOutdated(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Tenants []string `json:"tenants"`
		Apps    []string `json:"apps"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			s.fail(w, r, http.StatusBadRequest, `body is {"tenants": [...], "apps": [...]}, each optional`)
			return
		}
	}
	report, err := s.updateReport(r.Context(), "")
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	wanted := func(list []string, v string) bool {
		if len(list) == 0 {
			return true
		}
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	type outcome struct {
		Tenant  string `json:"tenant"`
		Profile string `json:"profile"`
		To      string `json:"to,omitempty"`
		Status  string `json:"status"`
		Detail  string `json:"detail,omitempty"`
		Message string `json:"message,omitempty"`
	}
	moved, skipped := []outcome{}, []outcome{}
	for _, e := range report.Entries {
		if !wanted(body.Tenants, e.Tenant) || !wanted(body.Apps, e.Profile) {
			continue
		}
		if retryable(e) {
			res, err := s.cfg.Repo.RetryMove(r.Context(), e.Tenant, e.Profile, e.Pinned, c.meta)
			if err != nil {
				skipped = append(skipped, outcome{Tenant: e.Tenant, Profile: e.Profile, To: e.Pinned, Status: "refused", Detail: err.Error()})
				continue
			}
			moved = append(moved, outcome{Tenant: e.Tenant, Profile: e.Profile, To: e.Pinned, Status: res.Status, Detail: res.Commit, Message: retryMessage(e.Rollout)})
			continue
		}
		if !e.Outdated {
			continue
		}
		if e.Rollout != nil && e.Rollout.Target == e.Newest && (e.Rollout.Phase == "Exporting" || e.Rollout.Phase == "Rolling") {
			skipped = append(skipped, outcome{Tenant: e.Tenant, Profile: e.Profile, To: e.Newest, Status: "under_way"})
			continue
		}
		res, detail, err := s.moveEntry(r.Context(), c, e)
		switch {
		case err != nil:
			skipped = append(skipped, outcome{Tenant: e.Tenant, Profile: e.Profile, To: e.Newest, Status: "refused", Detail: detail})
		default:
			moved = append(moved, outcome{Tenant: e.Tenant, Profile: e.Profile, To: e.Newest, Status: res.Status, Detail: res.Commit})
		}
	}
	s.json(w, http.StatusAccepted, map[string]any{"moved": moved, "skipped": skipped})
}

// moveEntry moves what one entry of the report names to its newest build:
// an app's own pin, or an add-on's pin in its app.
func (s *Server) moveEntry(ctx context.Context, c call, e updateEntry) (gitops.Result, string, error) {
	if e.AddonOf != "" {
		return s.moveAddonTo(ctx, c, e.Tenant, e.AddonOf, e.Profile, e.Catalogue, e.Newest)
	}
	return s.moveTo(ctx, c, e.Tenant, e.Profile, e.Catalogue, e.Newest, nil)
}

// moveTo moves one tenant's app to a build of a catalogue: fetched or read
// from the cluster's store, held to the floor, committed with its profile,
// and the pin moved. The same path an install takes, without a response
// writer: what would have been the refusal comes back as detail.
// defaultGrant is the install's, as InstallFrom takes it: nil for a move.
func (s *Server) moveTo(ctx context.Context, c call, tenant, profile, catalogueName, digest string, defaultGrant *bool) (gitops.Result, string, error) {
	source, detail, err := s.materialise(ctx, c, "app", tenant, profile, catalogueName, digest)
	if err != nil {
		return gitops.Result{}, detail, err
	}
	res, err := s.cfg.Repo.InstallFrom(ctx, tenant, profile, digest, source.Name, defaultGrant, c.meta)
	if err != nil {
		return gitops.Result{}, err.Error(), err
	}
	return res, "", nil
}

// moveAddonTo moves one add-on's pin in a tenant's app to a build of a
// catalogue, by the steps moveTo takes; the app's other add-ons, and who
// has each, stay as they are.
func (s *Server) moveAddonTo(ctx context.Context, c call, tenant, base, addon, catalogueName, digest string) (gitops.Result, string, error) {
	source, detail, err := s.materialise(ctx, c, "add-on", tenant, addon, catalogueName, digest)
	if err != nil {
		return gitops.Result{}, detail, err
	}
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil {
		return gitops.Result{}, err.Error(), err
	}
	for _, a := range apps {
		if a.Profile != base {
			continue
		}
		pin := []gitops.AddonPin{{Name: addon, Digest: digest, Catalogue: source.Name}}
		res, err := s.cfg.Repo.SetAddonsPinned(ctx, tenant, base, a.Addons, pin, nil, c.meta)
		if err != nil {
			return gitops.Result{}, err.Error(), err
		}
		return res, "", nil
	}
	return gitops.Result{}, "the tenant no longer has " + base + " installed", gitops.ErrNotInstalled
}

// materialise is what a move commits before the pin: the build fetched or
// read from the cluster's store, held to the floor and to the reserved
// addresses, and committed with its profile. what says which it is, "app"
// or "add-on", for a refusal.
func (s *Server) materialise(ctx context.Context, c call, what, tenant, profile, catalogueName, digest string) (visibleSource, string, error) {
	rec := &recorder{}
	req := (&http.Request{}).WithContext(ctx)
	source, known, err := s.resolveSource(ctx, tenant, catalogueName)
	if err != nil {
		return visibleSource{}, err.Error(), err
	}
	if !known {
		return visibleSource{}, "the tenant no longer sees catalog " + catalogueName, fmt.Errorf("unknown catalog")
	}
	fetched, ok := s.fetchOrStored(rec, req, source, tenant, profile, digest)
	if !ok {
		return visibleSource{}, rec.detail(), fmt.Errorf("fetch refused")
	}
	if s.refusedByFloor(rec, req, fetched, what+" "+catalogueName+"/"+profile) {
		return visibleSource{}, rec.detail(), fmt.Errorf("floor")
	}
	zone, err := s.reservedAddressZone(ctx)
	if err != nil {
		return visibleSource{}, err.Error(), err
	}
	if why := fetchedTakesReservedAddress(fetched.Definition, source.origin(tenant), zone); why != "" {
		return visibleSource{}, why, fmt.Errorf("reserved address")
	}
	if _, ok := s.commitEntry(rec, req, c, tenant, source, fetched); !ok {
		return visibleSource{}, rec.detail(), fmt.Errorf("commit refused")
	}
	return source, "", nil
}

// recorder captures what a handler would have answered, for a caller that
// is not answering a request.
type recorder struct {
	status int
	body   []byte
}

func (r *recorder) Header() http.Header         { return http.Header{} }
func (r *recorder) WriteHeader(code int)        { r.status = code }
func (r *recorder) Write(b []byte) (int, error) { r.body = append(r.body, b...); return len(b), nil }
func (r *recorder) detail() string {
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(r.body, &out); err == nil && out.Error != "" {
		return out.Error
	}
	return strings.TrimSpace(string(r.body))
}

// setAppUpdateMode writes a tenant's override of how an app moves: the
// cluster administrator's, under /v1/clusters, so that a tenant's
// administrator cannot give themselves the tenant mode.
func (s *Server) setAppUpdateMode(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body is {"mode": "latest" | "manual" | "tenant" | ""}; "" removes the override`)
		return
	}
	if body.Mode != "" && !gitops.ValidUpdateMode(body.Mode) {
		s.fail(w, r, http.StatusBadRequest, `mode is "latest", "manual" or "tenant"; "" removes the override`)
		return
	}
	res, err := s.cfg.Repo.SetAppUpdateMode(r.Context(), r.PathValue("t"), r.PathValue("p"), body.Mode, c.meta)
	if err != nil && strings.Contains(err.Error(), gitops.ErrNotInstalled.Error()) {
		s.fail(w, r, http.StatusNotFound, "the tenant does not have "+r.PathValue("p")+" installed")
		return
	}
	s.written(w, r, res, err)
}

// moveAllowed answers whether a move of an installed app under its mode is
// this caller's to make: a tenant-mode app is anybody's who may install in
// the tenant; under latest and manual the move is the cluster
// administrator's. Writes the refusal and answers false.
func (s *Server) moveAllowed(w http.ResponseWriter, r *http.Request, c call, tenant string, app gitops.App) bool {
	policy, err := s.cfg.Repo.UpdatePolicy(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return false
	}
	_, holds, _ := s.clusterSide(r.Context())
	mode, from := s.effectiveMode(policy, app, holds[app.Profile], tenant)
	if mode == string(gitops.UpdateTenant) {
		return true
	}
	ok, err := s.can(r.Context(), c, "can_configure", authz.Cluster(s.cfg.Cluster))
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return false
	}
	if ok {
		c.meta.Decision += " and can_configure " + authz.Cluster(s.cfg.Cluster)
		return true
	}
	why := "the cluster's administrator moves it"
	if mode == string(gitops.UpdateLatest) {
		why = "the platform moves it to the newest build inside the cluster's window"
	}
	s.fail(w, r, http.StatusForbidden, fmt.Sprintf(
		"%s moves to newer builds in %s mode (set by the %s): %s. Nothing was changed", app.Profile, mode, from, why))
	return false
}
