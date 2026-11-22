/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gentian-org/gentian-os/internal/tokenreview"
)

// The platform's path to the arbiter: what the operator writes into stores
// and reads out of them that no component's key may -- a store's export and
// its restore, a purge.
//
// It is admitted by workload identity, as the operator's own listener admits
// the director and the usher (internal/tokenreview): the
// operator presents a ServiceAccount token the kubelet projected into its
// pod for one audience, the arbiter's platform path, and the API server
// (TokenReview) says whether it is genuine, meant for that audience, and
// whose it is. Only the operator's ServiceAccount is admitted. A component
// holds no such token -- it could have one projected for this audience into
// its own pod, and that token names its own ServiceAccount, which is not
// the operator's -- and a component's key is not a token the API server
// vouches for. Nothing is stored or shared to make this work.

// PlatformAudience is the audience the operator's token for this path is
// projected for.
const PlatformAudience = "gentian-arbiter"

// PlatformAuth admits the operator, and nobody else, to the platform path.
type PlatformAuth struct {
	Reviews tokenreview.Reviewer
	// Caller is the operator's ServiceAccount as the API server names it:
	// system:serviceaccount:<namespace>:<name>. Empty admits nobody.
	Caller string

	once   sync.Once
	review *tokenreview.Cache
}

func (a *PlatformAuth) admit(ctx context.Context, header string) *refusal {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if a == nil || a.Reviews == nil || a.Caller == "" || !ok || token == "" {
		return &refusal{status: http.StatusUnauthorized, code: "unauthenticated", message: "the platform's token is required"}
	}
	a.once.Do(func() { a.review = tokenreview.New(a.Reviews, PlatformAudience) })
	username, err := a.review.Identify(ctx, token)
	switch {
	case errors.Is(err, tokenreview.ErrNotAuthenticated):
		return &refusal{status: http.StatusUnauthorized, code: "unauthenticated", message: "not a token for this path"}
	case err != nil:
		return &refusal{status: http.StatusServiceUnavailable, code: "unavailable", message: "the token could not be reviewed"}
	case username != a.Caller:
		return &refusal{status: http.StatusForbidden, code: "forbidden", message: "only the platform may use this path"}
	}
	return nil
}

// StoreExport is a store as a bundle carries it: its model and every tuple.
type StoreExport struct {
	Model  json.RawMessage `json:"model"`
	Tuples []Tuple         `json:"tuples"`
}

// PlatformHandler is the platform path's routes, behind auth.
func (s *Server) PlatformHandler(auth *PlatformAuth) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if ref := auth.admit(r.Context(), r.Header.Get("Authorization")); ref != nil {
				writeRefusal(w, ref)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16*maxBody)
			out, err := h(r)
			if err != nil {
				s.answerError(w, r, Entry{Tenant: r.PathValue("tenant"), Component: r.PathValue("component")}, err)
				return
			}
			if out == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		})
	}
	route("GET /v1/platform/stores/{tenant}/{component}", s.platformExport)
	route("PUT /v1/platform/stores/{tenant}/{component}", s.platformRestore)
	route("DELETE /v1/platform/stores/{tenant}/{component}", s.platformPurge)
	route("DELETE /v1/platform/tenants/{tenant}", s.platformPurgeTenant)
	return mux
}

func storeRef(r *http.Request) (string, string, error) {
	tenant, component := r.PathValue("tenant"), r.PathValue("component")
	if !dnsLabel.MatchString(tenant) || !dnsLabel.MatchString(component) {
		return "", "", badRequest("not a store: %q/%q", tenant, component)
	}
	return tenant, component, nil
}

func (s *Server) platformExport(r *http.Request) (any, error) {
	tenant, component, err := storeRef(r)
	if err != nil {
		return nil, err
	}
	model, tuples, found, err := s.stores.Export(r.Context(), tenant, component)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &refusal{status: http.StatusNotFound, code: "not_found", message: "no store is recorded for " + tenant + "/" + component}
	}
	return StoreExport{Model: model, Tuples: tuples}, nil
}

func (s *Server) platformRestore(r *http.Request) (any, error) {
	tenant, component, err := storeRef(r)
	if err != nil {
		return nil, err
	}
	var req StoreExport
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if len(req.Model) == 0 {
		return nil, badRequest("a restore carries the store's model")
	}
	for i, t := range req.Tuples {
		if err := t.valid(fmt.Sprintf("tuples[%d]", i)); err != nil {
			return nil, err
		}
	}
	return nil, s.stores.Restore(r.Context(), tenant, component, req.Model, req.Tuples)
}

func (s *Server) platformPurge(r *http.Request) (any, error) {
	tenant, component, err := storeRef(r)
	if err != nil {
		return nil, err
	}
	return nil, s.stores.Purge(r.Context(), tenant, component)
}

func (s *Server) platformPurgeTenant(r *http.Request) (any, error) {
	tenant := r.PathValue("tenant")
	if !dnsLabel.MatchString(tenant) {
		return nil, badRequest("not a tenant: %q", tenant)
	}
	return nil, s.stores.PurgeTenant(r.Context(), tenant)
}

// errForbiddenGroupTuple is a component writing a tuple about a group.
var errForbiddenGroupTuple = errors.New("tuples whose object is a group are the platform's")
