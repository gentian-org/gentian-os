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
	"slices"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/egress"
	"github.com/gentian-org/gentian-os/internal/security"
)

// What an egress grant answered, recorded.
//
// A grant of egress/<name> records what the approver answered: the hosts of
// a request by host, the digest of a request by address
// (security.EgressRuleDigest), or the hosts the approver named for a request
// whose hosts are named at approval. The cluster opens what the request asks
// now and the grant answered, and a request that asks for more waits for a
// new answer (security.GrantedEgress).
//
// The director records it wherever it writes a grant: an approval
// (privileges.go) and an approval given with an install (install_asks.go),
// each from the profile the install is at. A grant without a record answers
// the request as it is until the director records it; the pass below
// records it once, at start, from the profile the install is at then.

// egressRecordsPrincipal is who the pass's commits name.
const egressRecordsPrincipal = "platform:egress-records"

// installDefinition reads the profile of the build an install of tenant is
// at: an app's, or an addon's an app activates. Nil where the tenant has no
// such install or its build cannot be read.
func (s *Server) installDefinition(ctx context.Context, tenant, install string) (*gentianov1alpha1.ComponentProfile, error) {
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if errors.Is(err, gitops.ErrTenantNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.definitionOfInstall(ctx, apps, install), nil
}

// definitionOfInstall is installDefinition over apps already read.
func (s *Server) definitionOfInstall(ctx context.Context, apps []gitops.App, install string) *gentianov1alpha1.ComponentProfile {
	for _, a := range apps {
		if a.Profile == install {
			return s.definitionAt(ctx, a)
		}
	}
	for _, a := range apps {
		if !slices.Contains(a.Addons, install) {
			continue
		}
		addon := gitops.App{Profile: install}
		for _, pin := range a.AddonPins {
			if pin.Name == install {
				addon.Digest = pin.Digest
			}
		}
		return s.definitionAt(ctx, addon)
	}
	return nil
}

// egressRequestOf is the request ref names in definition, nil where it
// states none.
func egressRequestOf(definition *gentianov1alpha1.ComponentProfile, ref string) *gentianov1alpha1.EgressRequest {
	kind, name := security.SplitPrivilegeRef(ref)
	p := definition.Privileges()
	if kind != security.PrivilegeEgress || p == nil {
		return nil
	}
	for i := range p.Egress {
		if p.Egress[i].Name == name {
			return &p.Egress[i]
		}
	}
	return nil
}

// egressRecord is what a grant of e records when it is answered as it is:
// its hosts, each as egress.ParseHost writes it, once and sorted, or the
// digest of its rule. Nothing for a request whose hosts are named at
// approval: the approver names those.
func egressRecord(e *gentianov1alpha1.EgressRequest) (hosts []string, digest string, err error) {
	hosts, digest, err = security.EgressRecord(e)
	if err != nil {
		return nil, "", err
	}
	for i, h := range hosts {
		if parsed, perr := egress.ParseHost(h); perr == nil {
			hosts[i] = parsed.String()
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts), digest, nil
}

// egressRecordsRetry is how long the pass waits before it tries again a
// tenant it could not write: while a rollout puts this director beside an
// operator of another release, the repository refuses fields it cannot
// confirm, and the next try after the rollout succeeds.
var egressRecordsRetry = 5 * time.Minute

// startEgressRecords runs the pass beside the requests, when the program
// asks for it, and again while a tenant's grants could not be written: until
// one pass leaves nothing it could not write, or the director stops.
func (s *Server) startEgressRecords() {
	if !s.cfg.RecordEgressAnswers {
		return
	}
	s.watch(func(ctx context.Context) {
		for {
			if _, failed := s.recordEgressAnswers(ctx); failed == 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(egressRecordsRetry):
			}
		}
	})
}

// RecordEgressAnswers is the pass: for every tenant, every egress grant that
// records nothing and answers a request by host or by address of the
// profile its install is at gets that request's hosts or digest, in one
// commit per tenant. A grant whose install or request the director cannot
// read is left as it is. It writes nothing a second time: a grant that
// records its answer is not touched. It returns how many grants it recorded.
func (s *Server) RecordEgressAnswers(ctx context.Context) (recorded int) {
	recorded, _ = s.recordEgressAnswers(ctx)
	return recorded
}

// recordEgressAnswers is RecordEgressAnswers, also counting the tenants whose
// grants it could not read or write.
func (s *Server) recordEgressAnswers(ctx context.Context) (recorded, failed int) {
	log := s.cfg.Log.With("worker", "egress-records")
	tenants, err := s.cfg.Repo.Tenants(ctx)
	if err != nil {
		log.WarnContext(ctx, "the tenants could not be listed; no grant was recorded", "error", err.Error())
		return 0, 1
	}
	meta := gitops.Meta{Principal: egressRecordsPrincipal, Decision: "a grant records what it answered"}
	for _, tenant := range tenants {
		grants, err := s.cfg.Repo.TenantPrivileges(ctx, tenant)
		if err != nil {
			log.WarnContext(ctx, "the tenant's grants could not be read", "tenant", tenant, "error", err.Error())
			failed++
			continue
		}
		var apps []gitops.App
		var records []gitops.PrivilegeGrant
		for _, g := range grants {
			if kind, _ := security.SplitPrivilegeRef(g.Privilege); kind != security.PrivilegeEgress || len(g.Hosts) > 0 || g.Digest != "" {
				continue
			}
			if apps == nil {
				if apps, err = s.cfg.Repo.Apps(ctx, tenant); err != nil {
					log.WarnContext(ctx, "the tenant's apps could not be read", "tenant", tenant, "error", err.Error())
					failed++
					break
				}
			}
			e := egressRequestOf(s.definitionOfInstall(ctx, apps, g.Install), g.Privilege)
			if e == nil || e.HostsAtApproval {
				continue
			}
			hosts, digest, err := egressRecord(e)
			if err != nil || (len(hosts) == 0 && digest == "") {
				continue
			}
			g.Hosts, g.Digest = hosts, digest
			records = append(records, g)
		}
		if len(records) == 0 {
			continue
		}
		res, err := s.cfg.Repo.RecordPrivilegeAnswers(ctx, tenant, records, meta)
		if err != nil {
			log.WarnContext(ctx, "what the tenant's egress grants answered could not be recorded", "tenant", tenant, "error", err.Error())
			failed++
			continue
		}
		if res.Changed {
			recorded += len(records)
			log.InfoContext(ctx, "recorded what the tenant's egress grants answered", "tenant", tenant, "grants", len(records), "commit", res.Commit)
		}
	}
	return recorded, failed
}
