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

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/membership"
)

// Who is in which group is cluster state, so the operator projects it.
//
// It was the director's, and it was the last thing the director wrote to the
// authorization graph. That made the rule "the director reads the graph and
// writes git" untrue in exactly one place, and an exception in one place is a
// credential that has to stay powerful everywhere: as long as this endpoint
// lived in the director, the director's OpenFGA token needed write access, and
// nothing but care stopped the rest of it from using that.
//
// It is the same shape as the structure projection beside it. Keycloak is
// where a membership is changed; what the graph holds is a projection of that,
// fed by signed statements from a listener inside Keycloak and never edited in
// place. Events carry state and not deltas -- "this user's groups are now
// exactly these" -- so a statement delivered twice changes nothing and one
// that was lost is repaired by the next statement about the same user.
//
// Not a reconciler, because there is nothing to poll: Keycloak tells us. It
// rides the manager as a Runnable, the way the credential manager does, rather
// than being a second Deployment with a second identity to provision.

// MembershipListener serves the endpoint Keycloak's event listener posts to.
type MembershipListener struct {
	// Addr is what the HTTP server listens on.
	Addr string
	// Handler verifies a statement and applies it to the graph.
	Handler http.Handler
	// Log is the operator's logger, for the lines this server writes itself.
	Log *slog.Logger
}

// NewMembershipListener builds the listener from the environment, or returns
// nil when this cluster has not been given a key to believe.
//
// Nil rather than an error, and a warning rather than silence: a cluster with
// no listener key accepts no membership at all, so every permission that
// follows from being in a group is refused to everyone. That is a
// configuration a person may have chosen, and it is also exactly what a
// forgotten secret looks like, so it says so at start either way.
func NewMembershipListener(graph *authz.OpenFGA, logger *slog.Logger) (*MembershipListener, error) {
	if graph == nil {
		return nil, nil
	}
	raw := os.Getenv("GENTIAN_LISTENER_KEYS")
	if raw == "" {
		// The director's spelling, for a cluster mid-upgrade whose Secret is
		// still mounted under the old name. Both are the same value.
		raw = os.Getenv("DIRECTOR_LISTENER_KEYS")
	}
	if raw == "" {
		logger.Warn("no Keycloak listener key configured: membership statements are not accepted, so every permission that follows from a group is refused",
			"setting", "GENTIAN_LISTENER_KEYS")
		return nil, nil
	}
	keys, err := membership.ParseKeys(raw)
	if err != nil {
		return nil, fmt.Errorf("GENTIAN_LISTENER_KEYS: %w", err)
	}
	projector, err := membership.NewProjector(graph, membership.Scope{
		PlatformRealm:  envOrDefault("GENTIAN_PLATFORM_REALM", "kernel"),
		PlatformTenant: envOrDefault("GENTIAN_PLATFORM_TENANT", "platform"),
	}, logger)
	if err != nil {
		return nil, err
	}
	receiver, err := membership.NewReceiver(keys, projector, logger)
	if err != nil {
		return nil, err
	}
	return &MembershipListener{
		Addr:    envOrDefault("GENTIAN_MEMBERSHIP_ADDR", ":8090"),
		Handler: receiver,
		Log:     logger,
	}, nil
}

// Start runs the server until the manager's context is cancelled.
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

	srv := &http.Server{
		Addr:              l.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
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
		// A statement in flight is a user's whole group set; finishing it is
		// cheaper than making Keycloak resend it.
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
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
