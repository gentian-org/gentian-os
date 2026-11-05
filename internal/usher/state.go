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
	"regexp"

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
	// What newer builds of one app left behind on the cluster: the pieces
	// its bundle, or an add-on's, brought once and brings no longer. The
	// pieces are the cluster's, and the read is the tenant's all the same:
	// the operator answers it only for an app this tenant has, with the part
	// of the cluster's list that names that app, and says in the answer who
	// may remove them -- this tenant's administrator where the tenant is the
	// cluster's only one, the platform's otherwise. Removing is the
	// director's command either way.
	s.guarded("GET /v1/tenants/{t}/apps/{p}/residue", "can_view", tenantObject, s.appResidue)

	// A tenant's resources: the ceiling the cluster enforces, what is under
	// it, the plans it may move to, and its history.
	s.guarded("GET /v1/tenants/{t}/resources", "can_view", tenantObject, s.relay("/resources"))
	s.guarded("GET /v1/tenants/{t}/resources/plans", "can_view", tenantObject, s.resourcePlans)
	s.guarded("GET /v1/tenants/{t}/resources/usage", "can_view", tenantObject, s.relay("/resources/usage", "from", "to", "stepSeconds"))
	// What the tenant's public entries carried, for those who answer for the
	// perimeter: can_expose, the relation that publishes them.
	s.guarded("GET /v1/tenants/{t}/perimeter/traffic", "can_expose", tenantObject, s.relay("/perimeter/traffic", "from", "to", "stepSeconds"))
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
	// Where one restore stands. The director starts it and answers with its
	// name; what it did is read here, like a backup's.
	s.guarded("GET /v1/tenants/{t}/restores/{name}", "can_view", tenantObject, s.tenantRestore)

	// A tenant's objects of one add-on's type -- a backup schedule, say --
	// with what the add-on reports of each: declared through the director
	// under can_set_policy, read here by whoever may see the tenant, like
	// its backup policy. The operator answers only for a type an add-on
	// brought and holds a live grant for.
	s.guarded("GET /v1/tenants/{t}/extensions/{group}/{resource}", "can_view", tenantObject, s.tenantExtensionObjects)

	// What one tenant's apps consume from each other, and the notices
	// published to its people.
	s.guarded("GET /v1/tenants/{t}/integrations", "can_view", tenantObject, s.relay("/integrations"))
	s.guarded("GET /v1/tenants/{t}/notifications", "can_view", tenantObject, s.relay("/notifications"))

	if s.cfg.Cluster != "" {
		// The cluster's own state, read under can_audit: every tenant's
		// ceiling, what the platform keeps and for how long, what may escape
		// the default posture, and how much customization it carries.
		s.guarded("GET /v1/clusters/{c}/resources", "can_audit", clusterObject, s.clusterResources)
		// Every tenant's billed intervals over one window: what a commerce
		// backend invoices the cluster's tenants from.
		s.guarded("GET /v1/clusters/{c}/resources/report", "can_audit", clusterObject, s.clusterResourceReport)
		s.guarded("GET /v1/clusters/{c}/backup-policy", "can_audit", clusterObject, s.relayCluster("/v1/backup-policy"))
		s.guarded("GET /v1/clusters/{c}/backup-schedules", "can_audit", clusterObject, s.relayCluster("/v1/backup-schedules"))
		// The cluster's objects of one add-on's type: the platform tenant's,
		// where the director writes them under can_configure.
		s.guarded("GET /v1/clusters/{c}/extensions/{group}/{resource}", "can_audit", clusterObject, s.clusterExtensionObjects)
		s.guarded("GET /v1/clusters/{c}/platform-security", "can_audit", clusterObject, s.relayCluster("/v1/platform-security"))
		s.guarded("GET /v1/clusters/{c}/customizations", "can_audit", clusterObject, s.relayCluster("/v1/customizations"))
		// What the cluster last reported about itself, exactly as sent, and
		// what became of it; {"enabled":false} on a cluster that does not
		// report. The operator's answer, like the rest: it is the one that
		// sends.
		s.guarded("GET /v1/clusters/{c}/usage-statistics", "can_audit", clusterObject, s.relayCluster("/v1/usage-statistics"))
		// What the catalog left behind: objects a profile's bundle once
		// brought and no bundle owns now, and profiles nobody uses. They are
		// the cluster's and no tenant's -- most have no namespace -- so the
		// read is the platform's, under can_audit. Removing one is the
		// director's command.
		s.guarded("GET /v1/clusters/{c}/catalogue/residue", "can_audit", clusterObject, s.relayCluster("/v1/catalogue/residue"))
		// What awaits an approver: every tenant's pending privileges, each
		// naming who answers it -- the security officer for one that weakens
		// what protects the node, with who asked, and the perimeter approver
		// for egress. Read by the two who answer them (can_approve,
		// can_approve_perimeter) and by the cluster's administrator
		// (can_configure), who sees what the cluster is waiting on; the
		// director decides each approval again. Not can_audit: the list
		// names people and their notes, and an auditor reads the record of
		// what was decided, not the queue.
		s.guardedAny("GET /v1/clusters/{c}/approvals", []string{"can_approve", "can_approve_perimeter", "can_configure"}, clusterObject, s.relayCluster("/v1/approvals"))
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

func (s *Server) appResidue(w http.ResponseWriter, r *http.Request, c call) {
	s.relayed(w, r, tenantPath(c, "/apps/"+url.PathEscape(r.PathValue("p"))+"/residue"))
}

func (s *Server) tenantRestore(w http.ResponseWriter, r *http.Request, c call) {
	s.relayed(w, r, tenantPath(c, "/restores/"+url.PathEscape(r.PathValue("name"))))
}

// extensionPath is the operator's path below prefix for the add-on type in
// the request's path, or false for a group or resource that cannot be one.
func extensionPath(r *http.Request, prefix string) (string, bool) {
	group, resource := r.PathValue("group"), r.PathValue("resource")
	if !extensionGroup.MatchString(group) || !extensionResource.MatchString(resource) {
		return "", false
	}
	return prefix + "/extensions/" + url.PathEscape(group) + "/" + url.PathEscape(resource), true
}

// extensionGroup is a DNS subdomain and extensionResource a DNS label: what a
// type's group and plural are.
var (
	extensionGroup    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	extensionResource = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func (s *Server) tenantExtensionObjects(w http.ResponseWriter, r *http.Request, c call) {
	path, ok := extensionPath(r, tenantPath(c, ""))
	if !ok {
		fail(w, http.StatusBadRequest, "invalid name")
		return
	}
	s.relayed(w, r, path)
}

func (s *Server) clusterExtensionObjects(w http.ResponseWriter, r *http.Request, _ call) {
	path, ok := extensionPath(r, "/v1")
	if !ok {
		fail(w, http.StatusBadRequest, "invalid name")
		return
	}
	s.relayed(w, r, path)
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

// clusterResourceReport relays the operator's report of every tenant over
// the window asked, from and to and nothing else of the query.
func (s *Server) clusterResourceReport(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, "/v1/resources/report", "from", "to")
}

func (s *Server) unreachable(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Log.ErrorContext(r.Context(), "app-lifecycle API unreachable", "error", err.Error())
	fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
}

// resourcePlans answers the catalog as it applies to this tenant: the
// plans come from the operator, which marks the ones this tenant may not
// move to and why. Who moves it is the cluster's administrator; the tenant
// reads what there is.
func (s *Server) resourcePlans(w http.ResponseWriter, r *http.Request, c call) {
	plans, err := s.cfg.Lifecycle.Plans(r.Context(), c.tenant)
	if err != nil {
		var upstream *lifecycle.UpstreamError
		if errors.As(err, &upstream) {
			fail(w, r, upstream.Status, upstream.Message)
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
		fail(w, r, status, lifecycle.ErrorMessage(body))
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
