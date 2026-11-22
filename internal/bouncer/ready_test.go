/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// downFor is a Keycloak that refuses the first n fetches.
type downFor struct {
	n, calls atomic.Int32
	realm    atomic.Value
}

func (k *downFor) Warm(_ context.Context, realm string) error {
	k.realm.Store(realm)
	if k.calls.Add(1) <= k.n.Load() {
		return errors.New("connection refused")
	}
	return nil
}

func probe(r *Readiness) int {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

// A bouncer is not ready before it holds the realm's keys, is once it does,
// and stays ready after.
func TestABouncerIsReadyOnceItHoldsTheKeys(t *testing.T) {
	t.Parallel()
	keycloak := &downFor{}
	keycloak.n.Store(2)
	r := &Readiness{}
	if probe(r) != http.StatusServiceUnavailable {
		t.Fatal("ready before the keys were fetched")
	}
	done := make(chan struct{})
	go func() {
		WarmUntilReady(context.Background(), keycloak, "kernel", time.Millisecond, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never ready")
	}
	if probe(r) != http.StatusNoContent || keycloak.calls.Load() != 3 || keycloak.realm.Load() != "kernel" {
		t.Fatalf("probe %d after %d fetches of %v", probe(r), keycloak.calls.Load(), keycloak.realm.Load())
	}
}

// A bouncer that never reaches Keycloak stays not ready, and stops trying
// when it is told to stop.
func TestABouncerThatNeverGetsTheKeysIsNotReady(t *testing.T) {
	t.Parallel()
	keycloak := &downFor{}
	keycloak.n.Store(1 << 30)
	r := &Readiness{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	WarmUntilReady(ctx, keycloak, "kernel", time.Millisecond, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if probe(r) != http.StatusServiceUnavailable {
		t.Fatal("ready without the keys")
	}
}
