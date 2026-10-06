/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usher

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// Reads of live state: what the cluster holds of a tenant right now.
//
// The director relayed these once. They are not commits, and the process
// that holds the push credential should not be the one that answers for the
// cluster, so they are here, each under the relation and on the object the
// director asked.
//
// The answers are the operator's, asked for over its listener. They are not
// read from the cluster directly: every one of them is worked out inside the
// operator -- a ceiling paired with consumption, a policy with inheritance
// applied, an app's state from its pods, usage from the sampler's database,
// notices from the desktop's -- so reading the objects behind them would
// mean giving this ServiceAccount read access to tenants' pods, quotas and
// backups across the cluster, and a second copy of each computation. What
// the usher has instead is an identity the operator's listener admits to
// these reads and to nothing else (internal/applifecycle/auth.go): its own
// ServiceAccount, proven by a token issued for that listener alone, which
// the API server itself would not accept.
//
// That identity is not narrowed by tenant: with it this process can read any
// tenant's state. The guard is what keeps one tenant's from another, which
// is the director's position before the move and no better.
func (s *Server) stateRoutes() {
	// What the cluster made of what git says is installed.
	s.guarded("GET /v1/tenants/{t}/apps/status", "can_view", tenantObject, s.relay("/apps/status"))
	// What uninstalled apps still hold: the apps that are "retained", and
	// which kinds of data each one has left. Uninstalling keeps an app's
	// data, and this is the only place that says so afterwards.
	s.guarded("GET /v1/tenants/{t}/apps/retained", "can_view", tenantObject, s.relay("/apps/retained"))

	// A tenant's resources: the ceiling the cluster enforces, what is under
	// it, the plans it may move to, and its history.
	s.guarded("GET /v1/tenants/{t}/resources", "can_view", tenantObject, s.relay("/resources"))
	s.guarded("GET /v1/tenants/{t}/resources/plans", "can_view", tenantObject, s.resourcePlans)
	s.guarded("GET /v1/tenants/{t}/resources/usage", "can_view", tenantObject, s.relay("/resources/usage", "from", "to", "stepSeconds"))
	s.guarded("GET /v1/tenants/{t}/resources/report", "can_view", tenantObject, s.relay("/resources/report", "from", "to"))

	// A tenant's backups: what exists, what each run did, the policy in
	// force once inheritance is resolved, and when the next scheduled run
	// is. Whoever may see a tenant may see whether its data is being kept.
	// The bundle itself is not served here: the usher's identity does not
	// fetch one.
	s.guarded("GET /v1/tenants/{t}/backups", "can_view", tenantObject, s.relay("/backups"))
	s.guarded("GET /v1/tenants/{t}/backups/{name}", "can_view", tenantObject, s.tenantBackup)
	s.guarded("GET /v1/tenants/{t}/backup-policy", "can_view", tenantObject, s.relay("/backup-policy"))
	s.guarded("GET /v1/tenants/{t}/backup-schedules", "can_view", tenantObject, s.relay("/backup-schedules"))

	// What one tenant's apps consume from each other, and the notices
	// published to its people.
	s.guarded("GET /v1/tenants/{t}/integrations", "can_view", tenantObject, s.relay("/integrations"))
	s.guarded("GET /v1/tenants/{t}/notifications", "can_view", tenantObject, s.relay("/notifications"))

	if s.cfg.Cluster != "" {
		// The cluster's own state, read under can_audit: every tenant's
		// ceiling, what the platform keeps and for how long, what may escape
		// the default posture, and how much customisation it carries.
		s.guarded("GET /v1/clusters/{c}/resources", "can_audit", clusterObject, s.clusterResources)
		s.guarded("GET /v1/clusters/{c}/backup-policy", "can_audit", clusterObject, s.relayCluster("/v1/backup-policy"))
		s.guarded("GET /v1/clusters/{c}/backup-schedules", "can_audit", clusterObject, s.relayCluster("/v1/backup-schedules"))
		s.guarded("GET /v1/clusters/{c}/platform-security", "can_audit", clusterObject, s.relayCluster("/v1/platform-security"))
		s.guarded("GET /v1/clusters/{c}/customizations", "can_audit", clusterObject, s.relayCluster("/v1/customizations"))
		// What the cluster last reported about itself, exactly as sent, and
		// what became of it; {"enabled":false} on a cluster that does not
		// report. The operator's answer, like the rest: it is the one that
		// sends.
		s.guarded("GET /v1/clusters/{c}/licence-report", "can_audit", clusterObject, s.relayCluster("/v1/licence-report"))
	}
}

func tenantPath(c call, suffix string) string {
	return "/v1/tenants/" + url.PathEscape(c.tenant) + suffix
}

// relay answers a read about the tenant in the path with the operator's
// answer to the same question, passing only the query parameters named.
func (s *Server) relay(suffix string, allowed ...string) func(http.ResponseWriter, *http.Request, call) {
	return func(w http.ResponseWriter, r *http.Request, c call) {
		s.relayed(w, r, tenantPath(c, suffix), allowed...)
	}
}

// relayCluster answers a read about the cluster with the operator's answer
// at path.
func (s *Server) relayCluster(path string) func(http.ResponseWriter, *http.Request, call) {
	return func(w http.ResponseWriter, r *http.Request, _ call) {
		s.relayed(w, r, path)
	}
}

func (s *Server) tenantBackup(w http.ResponseWriter, r *http.Request, c call) {
	s.relayed(w, r, tenantPath(c, "/backups/"+url.PathEscape(r.PathValue("name"))))
}

// relayed passes the operator's answer to one read through as it came.
func (s *Server) relayed(w http.ResponseWriter, r *http.Request, path string, allowed ...string) {
	query := url.Values{}
	for _, name := range allowed {
		if v := r.URL.Query().Get(name); v != "" {
			query.Set(name, v)
		}
	}
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), path, query)
	if err != nil {
		s.unreachable(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) unreachable(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Log.ErrorContext(r.Context(), "app-lifecycle API unreachable", "error", err.Error())
	fail(w, http.StatusBadGateway, "the operator's API did not answer")
}

// resourcePlans answers the catalogue as it applies to this tenant and this
// caller. The plans come from the operator, which marks the ones this tenant
// may not move to and why. Whether the caller chooses for themselves, as a
// tenant's administrator does, or for the cluster is decided here from the
// store and never taken from the request: a plan the platform arranges by
// hand is withheld from whoever may not configure the cluster.
func (s *Server) resourcePlans(w http.ResponseWriter, r *http.Request, c call) {
	self := true
	if s.cfg.Cluster != "" {
		configures, err := s.cfg.Authz.Check(r.Context(), "", c.user, "can_configure", authz.Cluster(s.cfg.Cluster))
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		self = !configures
	}
	plans, err := s.cfg.Lifecycle.Plans(r.Context(), c.tenant, self)
	if err != nil {
		var upstream *lifecycle.UpstreamError
		if errors.As(err, &upstream) {
			fail(w, upstream.Status, upstream.Message)
			return
		}
		s.unreachable(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": c.tenant, "plans": plans})
}

// clusterResources answers every tenant's state, for the view that shows the
// cluster's ceilings side by side. The tenants are the ones the cluster
// holds, which is what the operator can speak for; one that is declared in
// git and not yet applied is not in this list.
func (s *Server) clusterResources(w http.ResponseWriter, r *http.Request, _ call) {
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), "/v1/resources", nil)
	if err != nil {
		s.unreachable(w, r, err)
		return
	}
	var answer struct {
		Tenants     []json.RawMessage `json:"tenants"`
		Unavailable []json.RawMessage `json:"unavailable"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &answer) != nil {
		if status == http.StatusOK {
			status = http.StatusBadGateway
		}
		fail(w, status, lifecycle.ErrorMessage(body))
		return
	}
	if answer.Tenants == nil {
		answer.Tenants = []json.RawMessage{}
	}
	if answer.Unavailable == nil {
		answer.Unavailable = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cluster": s.cfg.Cluster, "tenants": answer.Tenants, "unavailable": answer.Unavailable,
	})
}
