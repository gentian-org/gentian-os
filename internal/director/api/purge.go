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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Purging a tenant: retiring it with its data.
//
// Retire alone removes the tenant's directory, and the manifest's
// deletionPolicy decides what the operator does with the data -- Retain, as
// every tenant starts, keeps the realm, the databases and the namespace. A
// purge switches that to Delete first and removes the directory only once the
// live Tenant carries it. Argo CD applies only what git says at the moment it
// syncs, so both commits landing before one sync would prune the Tenant as
// the cluster last saw it, and the data the purge was for would stay.

var (
	// purgePoll is how often the cluster is asked whether it has taken in
	// Delete; purgeWait is how long one attempt waits before giving up until
	// the next restart or the next request.
	purgePoll = 10 * time.Second
	purgeWait = 30 * time.Minute
)

// purgeTenant asks for a tenant to be purged and answers once the first
// commit has landed; the second follows when the cluster is ready for it.
func (s *Server) purgeTenant(w http.ResponseWriter, r *http.Request, c call) {
	if s.cfg.Lifecycle == nil {
		s.fail(w, r, http.StatusServiceUnavailable,
			"this director cannot see the cluster, and a purge has to wait for it: nothing was changed")
		return
	}
	tenant := r.PathValue("t")
	// An empty body is the default purge; {"keepBundles": true} spares the
	// backup bucket.
	var opts gitops.PurgeOptions
	if r.ContentLength != 0 {
		if err := decode(r, &opts); err != nil {
			s.fail(w, r, http.StatusBadRequest, `body must be {} or {"keepBundles": true}`)
			return
		}
	}
	res, err := s.cfg.Repo.RequestTenantPurge(r.Context(), tenant, time.Now(), opts, c.meta)
	if errors.Is(err, gitops.ErrTenantProtected) {
		s.fail(w, r, http.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.finishPurgeLater(tenant, c.meta)
	s.json(w, http.StatusAccepted, map[string]any{"status": res.Status, "commit": res.Commit})
}

// ResumePurges picks up every purge a previous run asked for and did not
// finish. The manifest's purge annotation is the record of it, so a restart
// mid-purge loses nothing.
func (s *Server) ResumePurges(ctx context.Context) {
	if s.cfg.Lifecycle == nil {
		return
	}
	pending, err := s.cfg.Repo.PendingPurges(ctx)
	if err != nil {
		s.cfg.Log.Warn("pending purges could not be read", "error", err.Error())
		return
	}
	for _, tenant := range pending {
		s.cfg.Log.Info("resuming the purge of a tenant", "tenant", tenant)
		s.finishPurgeLater(tenant, gitops.Meta{
			Principal: "gentian-director",
			Decision:  "completes the purge requested in this tenant's manifest",
		})
	}
}

// finishPurgeLater runs the second half in the background, once per tenant.
func (s *Server) finishPurgeLater(tenant string, meta gitops.Meta) {
	if _, busy := s.purging.LoadOrStore(tenant, struct{}{}); busy {
		return
	}
	s.watch(func(background context.Context) {
		defer s.purging.Delete(tenant)
		ctx, cancel := context.WithTimeout(background, purgeWait)
		defer cancel()
		if err := s.finishPurge(ctx, tenant, meta); err != nil && background.Err() == nil {
			s.cfg.Log.Error("the purge did not finish", "tenant", tenant,
				"request_id", meta.RequestID, "error", err.Error())
		}
	})
}

func (s *Server) finishPurge(ctx context.Context, tenant string, meta gitops.Meta) error {
	for {
		ready, err := s.clusterHoldsDelete(ctx, tenant)
		if err != nil {
			s.cfg.Log.Warn("asking the cluster about a purge", "tenant", tenant, "error", err.Error())
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the cluster did not take in deletionPolicy: Delete within %s; "+
				"the purge resumes when the director restarts or is asked again", purgeWait)
		case <-time.After(purgePoll):
		}
	}
	// Not interrupted by the server closing: a commit is made whole or not
	// begun, and git has its own bound.
	res, err := s.cfg.Repo.RetireTenant(context.WithoutCancel(ctx), tenant, meta)
	if errors.Is(err, gitops.ErrTenantNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	s.cfg.Log.Info("tenant purged from git; the operator deletes its data", "tenant", tenant,
		"commit", res.Commit, "request_id", meta.RequestID)
	return nil
}

// clusterHoldsDelete is whether removing the manifest now deletes the data:
// the live Tenant says Delete, or there is no live Tenant and so no data.
func (s *Server) clusterHoldsDelete(ctx context.Context, tenant string) (bool, error) {
	code, body, err := s.cfg.Lifecycle.Get(ctx, "/v1/tenants/"+url.PathEscape(tenant), nil)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("the operator answered %d", code)
	}
	var state struct {
		Exists         bool   `json:"exists"`
		DeletionPolicy string `json:"deletionPolicy"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return false, err
	}
	return !state.Exists || state.DeletionPolicy == "Delete", nil
}
