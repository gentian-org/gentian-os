/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// A tenant's resources.
//
// The cluster knows the ceiling it enforces and what is committed under it;
// git knows which plan was chosen. This file joins the two the way every
// other route does: the reads are relayed from the operator once the caller
// is allowed them, and the write is a commit the caller is allowed to make.
// The operator has no write to call. It reads the plan back from git once
// Argo CD has synced it, like every other change to a tenant.

// relayed passes the operator's answer to one read through as it came, with
// only the query parameters the route allows.
func (s *Server) relayed(w http.ResponseWriter, r *http.Request, path string, allowed ...string) {
	query := url.Values{}
	for _, name := range allowed {
		if v := r.URL.Query().Get(name); v != "" {
			query.Set(name, v)
		}
	}
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), path, query)
	if err != nil {
		s.cfg.Log.ErrorContext(r.Context(), "app-lifecycle API unreachable", "request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func resourcesPath(r *http.Request, suffix string) string {
	return "/v1/tenants/" + url.PathEscape(r.PathValue("t")) + "/resources" + suffix
}

func (s *Server) resourceState(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, resourcesPath(r, ""))
}

func (s *Server) resourceUsage(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, resourcesPath(r, "/usage"), "from", "to", "stepSeconds")
}

func (s *Server) resourceReport(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, resourcesPath(r, "/report"), "from", "to")
}

// selfService reports whether the caller chooses for themselves, as a tenant
// administrator does, rather than for the cluster, as its operator does.
// Decided here from the graph, not asserted by a front end: a plan the
// platform arranges by hand is withheld from whoever may not configure the
// cluster, whatever their screen claimed.
func (s *Server) selfService(r *http.Request, c call) (bool, error) {
	if s.cfg.Cluster == "" {
		return true, nil
	}
	configures, err := s.cfg.Authz.Check(r.Context(), reqID(r.Context()), c.user, "can_configure", authz.Cluster(s.cfg.Cluster))
	if err != nil {
		return false, err
	}
	return !configures, nil
}

// resourcePlans answers the catalogue as it applies to this tenant and this
// caller. The plans come from the operator, which marks the ones this tenant
// may not move to and why; whether the caller is self-service is decided
// here.
func (s *Server) resourcePlans(w http.ResponseWriter, r *http.Request, c call) {
	self, err := s.selfService(r, c)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return
	}
	plans, err := s.cfg.Lifecycle.Plans(r.Context(), r.PathValue("t"), self)
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "plans": plans})
}

type setResourcePlanRequest struct {
	Plan string `json:"plan"`
	// Force skips the downgrade guard: the plan is written although what the
	// tenant has committed does not fit under it. Reserved for whoever may
	// configure the cluster, who has decided the tenant will be shrunk and
	// accepts the pods that will not come back as a known cost.
	Force bool `json:"force,omitempty"`
}

// setResourcePlan writes the chosen plan to git.
//
// The choice is validated against the operator's answer for this tenant: a
// plan the catalogue does not have is 404, one this tenant may not move to is
// refused with the operator's reason, and one the tenant's committed usage
// does not fit under is 409 unless forced. What is written is the plan's
// own quantities, so the ceiling the tenant ends up on is exactly one the
// platform has priced.
func (s *Server) setResourcePlan(w http.ResponseWriter, r *http.Request, c call) {
	var body setResourcePlanRequest
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Plan) == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"plan": "<name>"}`)
		return
	}
	self, err := s.selfService(r, c)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return
	}
	if body.Force && self {
		s.fail(w, r, http.StatusForbidden, "forcing a plan that does not fit is for whoever may configure the cluster")
		return
	}
	tenant := r.PathValue("t")
	plans, err := s.cfg.Lifecycle.Plans(r.Context(), tenant, self)
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	var chosen *lifecycle.Plan
	previous := ""
	for i := range plans {
		if plans[i].Current {
			previous = plans[i].Name
		}
		if plans[i].Name == body.Plan {
			chosen = &plans[i]
		}
	}
	if chosen == nil {
		s.fail(w, r, http.StatusNotFound, "no such resource plan on this cluster")
		return
	}
	if !chosen.Selectable {
		switch chosen.BlockedBy {
		case "fit":
			if !body.Force {
				// 409, not 400: the request is well formed and would be
				// accepted once the tenant frees something, or forced.
				s.fail(w, r, http.StatusConflict, chosen.Blocked)
				return
			}
		default:
			// Above the entitlement, or arranged by hand: what is missing
			// is an entitlement, not a permission, the same answer the
			// store gives for an app the tenant has not bought.
			s.fail(w, r, http.StatusPaymentRequired, chosen.Blocked)
			return
		}
	}
	res, err := s.cfg.Repo.SetResourcePlan(r.Context(), tenant, gitops.Plan{Name: chosen.Name, Quotas: chosen.Quotas}, c.meta)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	answer := map[string]any{"status": res.Status, "tenant": tenant, "plan": chosen.Name, "previousPlan": previous}
	if !res.Changed {
		answer["message"] = "the tenant is already on this plan"
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	answer["message"] = "committed to the deployments repository; Argo CD applies it on the next sync"
	s.json(w, http.StatusAccepted, answer)
}

// clusterResources answers every tenant's state, for the view that shows the
// cluster's ceilings side by side. The tenants are git's list; each state is
// the operator's answer. A tenant the operator cannot answer for -- one it
// has not provisioned yet, say -- is named under unavailable rather than
// dropped, so the view never silently shows fewer tenants than exist.
func (s *Server) clusterResources(w http.ResponseWriter, r *http.Request, _ call) {
	names, err := s.cfg.Repo.Tenants(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	states := make([]json.RawMessage, 0, len(names))
	unavailable := make([]map[string]string, 0)
	for _, name := range names {
		status, body, err := s.cfg.Lifecycle.Get(r.Context(), "/v1/tenants/"+url.PathEscape(name)+"/resources", nil)
		if err != nil {
			s.cfg.Log.ErrorContext(r.Context(), "app-lifecycle API unreachable", "request_id", reqID(r.Context()), "error", err.Error())
			s.fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
			return
		}
		if status != http.StatusOK || !json.Valid(body) {
			unavailable = append(unavailable, map[string]string{"tenant": name, "reason": lifecycle.ErrorMessage(body)})
			continue
		}
		states = append(states, json.RawMessage(body))
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "tenants": states, "unavailable": unavailable})
}

// lifecycleError answers a failed question to the operator. Its refusals
// carry over -- an unknown tenant is its 400 -- and its absence is a 502
// that says so.
func (s *Server) lifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	var upstream *lifecycle.UpstreamError
	if errors.As(err, &upstream) {
		s.fail(w, r, upstream.Status, upstream.Message)
		return
	}
	s.cfg.Log.ErrorContext(r.Context(), "app-lifecycle API unreachable", "request_id", reqID(r.Context()), "error", err.Error())
	s.fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
}

// A tenant's backups, relayed from the operator.
//
// What exists, what each run did, what policy is in force and when the next
// scheduled run is are all cluster state, held on the CRs the backup
// reconcilers own. The inheritance — a tenant's policy over the cluster's —
// is resolved there and reported on status, so it is relayed rather than
// recomputed here: two answers to "what applies to this tenant" is exactly
// the drift this architecture exists to avoid.

func backupsPath(r *http.Request, suffix string) string {
	return "/v1/tenants/" + url.PathEscape(r.PathValue("t")) + suffix
}

func (s *Server) tenantBackups(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, backupsPath(r, "/backups"))
}

func (s *Server) tenantBackup(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, backupsPath(r, "/backups/"+url.PathEscape(r.PathValue("name"))))
}

func (s *Server) tenantBackupPolicy(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, backupsPath(r, "/backup-policy"))
}

func (s *Server) tenantBackupSchedules(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, backupsPath(r, "/backup-schedules"))
}

func (s *Server) clusterBackupPolicy(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, "/v1/backup-policy")
}

func (s *Server) clusterBackupSchedules(w http.ResponseWriter, r *http.Request, _ call) {
	s.relayed(w, r, "/v1/backup-schedules")
}

// ── Backup: declared state, and actions ─────────────────────────────────────

// setTenantBackupPolicy writes what should be true of this tenant's backups
// from now on. A commit, like every other change to declared state.
func (s *Server) setTenantBackupPolicy(w http.ResponseWriter, r *http.Request, c call) {
	var body gitops.BackupPolicy
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, "body must be a backup policy")
		return
	}
	res, err := s.cfg.Repo.SetTenantBackupPolicy(r.Context(), r.PathValue("t"), body, c.meta)
	s.written(w, r, res, err)
}

// clearTenantBackupPolicy stops this tenant declaring one, which is how it
// goes back to the cluster's. There is no "inherit" value to write: a tenant
// inherits by saying nothing.
func (s *Server) clearTenantBackupPolicy(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.ClearTenantBackupPolicy(r.Context(), r.PathValue("t"), c.meta)
	s.written(w, r, res, err)
}

func (s *Server) setClusterBackupPolicy(w http.ResponseWriter, r *http.Request, c call) {
	var body gitops.BackupPolicy
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, "body must be a backup policy")
		return
	}
	res, err := s.cfg.Repo.SetClusterBackupPolicy(r.Context(), body, c.meta)
	s.written(w, r, res, err)
}

// startBackup asks the cluster to take one now.
func (s *Server) startBackup(w http.ResponseWriter, r *http.Request, c call) {
	var body map[string]any
	if err := decode(r, &body); err != nil {
		body = map[string]any{}
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(),
		"/v1/tenants/"+url.PathEscape(r.PathValue("t"))+"/actions/backup", c.meta.ActorName(), body)
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}

// deleteBackup removes one run's record. Also an action: the bundle's fate is
// the retention policy's, and nothing about the tenant's desired state changes.
func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil || body.Name == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"name": "<backup>"}`)
		return
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(),
		"/v1/tenants/"+url.PathEscape(r.PathValue("t"))+"/actions/delete-backup",
		c.meta.ActorName(), map[string]string{"name": body.Name})
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}

// ── The realm policy a tenant runs under ────────────────────────────────────

// tenantSecurityPolicy answers what the tenant declares.
//
// From git rather than from Keycloak, and that is not a shortcut: the
// composition writes these fields onto the realm on every reconcile, so what
// git says is what the realm is. Asking Keycloak instead would need a
// credential the director does not hold and should not.
func (s *Server) tenantSecurityPolicy(w http.ResponseWriter, r *http.Request, _ call) {
	policy, err := s.cfg.Repo.TenantSecurityPolicy(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	// Nothing declared is a real answer, and an empty object is how it is
	// said: the screen renders the defaults rather than a blank form.
	if policy == nil {
		policy = &gitops.SecurityPolicy{}
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": r.PathValue("t"),
		"policy": policy,
		// What the realm does when the tenant states nothing. Not a guess:
		// these are the values crossplane/compositions/tenant-default.yaml
		// writes, and they are here so a console can say "12 hours, unless
		// you change it" instead of leaving a zero to be interpreted.
		"defaults": map[string]any{"session": map[string]any{"idleMinutes": 720, "maxHours": 12}},
	})
}

// setTenantSecurityPolicy commits it.
func (s *Server) setTenantSecurityPolicy(w http.ResponseWriter, r *http.Request, c call) {
	var body gitops.SecurityPolicy
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, "body must be a security policy")
		return
	}
	res, err := s.cfg.Repo.SetTenantSecurityPolicy(r.Context(), r.PathValue("t"), body, c.meta)
	s.written(w, r, res, err)
}

// ── What changed, and under what authority ──────────────────────────────────

// changeWindow reads the two parameters a change list takes.
func changeWindow(r *http.Request) (int, string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return limit, r.URL.Query().Get("since")
}

// tenantChanges answers a tenant's history from git.
//
// Nothing is stored for this. The commits are the record, the trailer is the
// authority, and the answer marks a commit that carries no trailer as what it
// is: a change pushed by hand, which is the thing an audit most wants to see
// rather than have smoothed over.
func (s *Server) tenantChanges(w http.ResponseWriter, r *http.Request, _ call) {
	limit, since := changeWindow(r)
	changes, err := s.cfg.Repo.TenantChanges(r.Context(), r.PathValue("t"), limit, since)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant":  r.PathValue("t"),
		"changes": changes,
		// Said in the answer, because a screen headed "audit" that showed
		// only this would be claiming more than it has.
		"covers": "changes to declared state, from git. Sign-ins, refused requests and reads of data are recorded elsewhere and are not in this list.",
	})
}

func (s *Server) clusterChanges(w http.ResponseWriter, r *http.Request, _ call) {
	limit, since := changeWindow(r)
	changes, err := s.cfg.Repo.ClusterChanges(r.Context(), limit, since)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{
		"cluster": s.cfg.Cluster,
		"changes": changes,
		"covers":  "changes to declared state, from git. Sign-ins, refused requests and reads of data are recorded elsewhere and are not in this list.",
	})
}
