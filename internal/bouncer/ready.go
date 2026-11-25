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
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Warmer fetches a realm's signing keys ahead of the first token
// (authn.Verifier).
type Warmer interface {
	Warm(ctx context.Context, realm string) error
}

// Readiness is whether this bouncer holds the realm's signing keys, which it
// cannot check a session without. A new pod is not given requests before it
// does: in a rollout the pods before it keep answering, keys in hand, while
// Keycloak is restarting too.
//
// Once ready it stays ready. Keycloak going away later is answered with 503
// for a token whose keys are not at hand; a bouncer taken out of the Service
// then would leave the Gateway with nobody to ask at all.
type Readiness struct{ ready atomic.Bool }

// Ready reports whether the keys have been fetched once.
func (r *Readiness) Ready() bool { return r.ready.Load() }

// ServeHTTP answers a readiness probe: 204 when ready, 503 before.
func (r *Readiness) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if r.Ready() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "the realm's signing keys are not fetched yet", http.StatusServiceUnavailable)
}

// WarmUntilReady fetches the realm's keys, every interval until Keycloak
// answers, and then marks r ready. It returns when it has, or when ctx ends.
func WarmUntilReady(ctx context.Context, w Warmer, realm string, every time.Duration, r *Readiness, log *slog.Logger) {
	for {
		err := w.Warm(ctx, realm)
		if err == nil {
			r.ready.Store(true)
			log.Info("signing keys fetched; ready", "realm", realm)
			return
		}
		log.Warn("signing keys not fetched yet; not ready", "realm", realm, "error", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
