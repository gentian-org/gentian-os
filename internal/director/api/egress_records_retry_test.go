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
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// refusingOnce is a repository whose tenant's grants cannot be read the
// first time, as while a rollout puts the director beside an operator of
// another release, and hold nothing to record after that.
type refusingOnce struct {
	Repository
	reads atomic.Int32
}

func (r *refusingOnce) Tenants(context.Context) ([]string, error) { return []string{"demo"}, nil }

func (r *refusingOnce) TenantPrivileges(context.Context, string) ([]gitops.PrivilegeGrant, error) {
	if r.reads.Add(1) == 1 {
		return nil, errors.New("not yet")
	}
	return nil, nil
}

// A tenant the pass could not finish is tried again after a while, and the
// pass ends once nothing is left that it could not do.
func TestTheRecordPassTriesAgainUntilEveryTenantIsDone(t *testing.T) {
	defer func(d time.Duration) { egressRecordsRetry = d }(egressRecordsRetry)
	egressRecordsRetry = time.Millisecond
	repo := &refusingOnce{}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	s := &Server{
		cfg:        Config{Repo: repo, RecordEgressAnswers: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		background: ctx, stop: stop,
	}
	s.startEgressRecords()
	done := make(chan struct{})
	go func() { s.watchers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not end once every tenant was done")
	}
	if n := repo.reads.Load(); n != 2 {
		t.Fatalf("the tenant's grants were read %d times, want 2: once refused, once done", n)
	}
}
