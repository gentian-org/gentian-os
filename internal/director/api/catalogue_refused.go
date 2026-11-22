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
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// What an earlier release wrote into the catalog directory in a shape this
// one no longer has (a removed class or field) is taken out of git, so that
// the directory applies again (gitops.TakeOutRefused); and a field this one
// no longer has is taken out of every tenant's file, so that the tenant
// applies again (gitops.TakeOutUndeclared). At start, after a release brought
// a new schema, and every so often after that.
const (
	refusedEvery        = 30 * time.Minute
	refusedPrincipal    = "platform:catalogue"
	undeclaredPrincipal = "platform:tenants"
)

// startTakeOutRefused runs the pass beside the requests.
func (s *Server) startTakeOutRefused() {
	if !s.cfg.TakeOutRefused {
		return
	}
	s.watch(func(ctx context.Context) {
		s.TakeOutRefused(ctx)
		s.TakeOutUndeclared(ctx)
		t := time.NewTicker(refusedEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.TakeOutRefused(ctx)
				s.TakeOutUndeclared(ctx)
			}
		}
	})
}

// TakeOutRefused is one pass: how many files it took out.
func (s *Server) TakeOutRefused(ctx context.Context) int {
	set := schemacheck.Embedded()
	meta := gitops.Meta{Principal: refusedPrincipal, Decision: "this release's schema refuses them"}
	res, why, err := s.cfg.Repo.TakeOutRefused(ctx, set.Removed, meta)
	if err != nil {
		s.cfg.Log.Warn("what the schema refuses could not be taken out of the catalog directory", "error", err.Error())
		return 0
	}
	for file, reasons := range why {
		s.cfg.Log.Info("taken out of the catalog directory: the schema refuses it", "file", file, "why", reasons, "commit", res.Commit)
	}
	return len(why)
}

// TakeOutUndeclared is one pass over the tenants' files: how many it changed.
func (s *Server) TakeOutUndeclared(ctx context.Context) int {
	meta := gitops.Meta{Principal: undeclaredPrincipal, Decision: "this release's schema does not declare them"}
	res, taken, left, err := s.cfg.Repo.TakeOutUndeclared(ctx, schemacheck.Embedded().Undeclared, meta)
	if err != nil {
		s.cfg.Log.Warn("what the schema does not declare could not be taken out of the tenants' files", "error", err.Error())
		return 0
	}
	for file, fields := range left {
		s.cfg.Log.Warn("a tenant's file names fields the schema does not declare, and they could not be taken out: the tenant is not applied until they are removed",
			"file", file, "fields", fields)
	}
	for file, fields := range taken {
		s.cfg.Log.Info("taken out of a tenant's file: the schema does not declare it", "file", file, "fields", fields, "commit", res.Commit)
	}
	return len(taken)
}
