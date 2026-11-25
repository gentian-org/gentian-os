/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/envvar"
	"github.com/gentian-org/gentian-os/internal/membership"
)

// What only a realm sees happen to a person reaches the operator here: that
// somebody came in through an identity provider, and that somebody was
// removed. Who holds what is the rights store's, written by the operator
// alone; these two statements are what it cannot learn any other way, and
// the listener inside Keycloak signs each.
//
// Not a reconciler, because there is nothing to poll: Keycloak tells us. It
// rides the manager as a Runnable rather than being a second Deployment with
// a second identity to provision.

// MembershipListener serves the endpoint Keycloak's event listener posts to.
type MembershipListener struct {
	// Addr is what the HTTP server listens on.
	Addr string
	// Handler verifies a statement and applies it.
	Handler http.Handler
	// Log is the operator's logger, for the lines this server writes itself.
	Log *slog.Logger
}

// NewMembershipListener builds the listener from the environment, or returns
// nil when this cluster has not been given a key to believe.
//
// Nil rather than an error, and a warning rather than silence: without a
// listener key nobody who arrives from another realm is given anything, and
// nothing of a removed person is taken. That is exactly what a forgotten
// secret looks like, so it says so at start.
func NewMembershipListener(arrivals membership.Arrivals, logger *slog.Logger, departures ...membership.Departures) (*MembershipListener, error) {
	raw := os.Getenv("GENTIAN_LISTENER_KEYS")
	if raw == "" {
		logger.Warn("no Keycloak listener key configured: nothing follows from a person arriving from another realm or being removed",
			"setting", "GENTIAN_LISTENER_KEYS")
		return nil, nil
	}
	keys, err := membership.ParseKeys(raw)
	if err != nil {
		return nil, fmt.Errorf("GENTIAN_LISTENER_KEYS: %w", err)
	}
	receiver, err := membership.NewReceiver(keys, arrivals, logger, departures...)
	if err != nil {
		return nil, err
	}
	return &MembershipListener{
		Addr:    envvar.Or("GENTIAN_MEMBERSHIP_ADDR", ":8090"),
		Handler: receiver,
		Log:     logger,
	}, nil
}

// Start runs the server until the manager's context is canceled.
func (l *MembershipListener) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	// The path Keycloak's listener is configured with. Kept as it was in the
	// director so that moving this does not need the listener's configuration
	// to change in the same breath: only the host it points at changes.
	mux.Handle("POST /v1/events/keycloak", l.Handler)
	// Something to check without making a statement.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := newMembershipServer(l.Addr, mux)
	errs := make(chan error, 1)
	go func() {
		log.FromContext(ctx).Info("membership listener accepting statements from Keycloak", "addr", l.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		// Finishing a statement in flight is cheaper than making Keycloak
		// resend it.
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// newMembershipServer bounds what one connection can hold: the headers'
// size, and how long reading a request, answering it and waiting for the
// next may take. A statement is at most 256 KiB and is applied in one pass,
// so none of these is near what Keycloak's sender needs.
func newMembershipServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
}

// NeedLeaderElection keeps one replica answering.
//
// Statements are idempotent, so two replicas applying the same one would be
// harmless, but only one of them can be behind a Service endpoint that
// Keycloak has a single URL for, and a statement applied by a replica that is
// about to lose leadership is a write from a process that should not be
// writing.
func (l *MembershipListener) NeedLeaderElection() bool { return true }
