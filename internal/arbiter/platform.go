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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The platform's path to the arbiter: what the operator writes into stores
// and reads out of them that no component's key may -- the realm's group
// memberships, a store's export and its restore, a purge.
//
// It is admitted by workload identity, as the operator's own listener admits
// the director and the usher (internal/applifecycle, CallerAuth): the
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

// TokenReviewer asks the API server about a token: the one method of
// client-go's TokenReviewInterface.
type TokenReviewer interface {
	Create(ctx context.Context, review *authenticationv1.TokenReview, opts metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// PlatformAuth admits the operator, and nobody else, to the platform path.
type PlatformAuth struct {
	Reviews TokenReviewer
	// Caller is the operator's ServiceAccount as the API server names it:
	// system:serviceaccount:<namespace>:<name>. Empty admits nobody.
	Caller string

	mu    sync.Mutex
	cache map[[sha256.Size]byte]time.Time
}

// platformReviewTTL is how long a token the API server vouched for is
// admitted without asking again.
const platformReviewTTL = 30 * time.Second

func (a *PlatformAuth) admit(ctx context.Context, header string) *refusal {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if a == nil || a.Reviews == nil || a.Caller == "" || !ok || token == "" {
		return &refusal{status: http.StatusUnauthorized, code: "unauthenticated", message: "the platform's token is required"}
	}
	sum := sha256.Sum256([]byte(token))
	a.mu.Lock()
	until, cached := a.cache[sum]
	a.mu.Unlock()
	if cached && time.Now().Before(until) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	review, err := a.Reviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{PlatformAudience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return &refusal{status: http.StatusServiceUnavailable, code: "unavailable", message: "the token could not be reviewed"}
	}
	if !review.Status.Authenticated || !audienceIn(review.Status.Audiences) {
		return &refusal{status: http.StatusUnauthorized, code: "unauthenticated", message: "not a token for this path"}
	}
	if review.Status.User.Username != a.Caller {
		return &refusal{status: http.StatusForbidden, code: "forbidden", message: "only the platform may use this path"}
	}
	a.mu.Lock()
	if a.cache == nil || len(a.cache) > 64 {
		a.cache = map[[sha256.Size]byte]time.Time{}
	}
	a.cache[sum] = time.Now().Add(platformReviewTTL)
	a.mu.Unlock()
	return nil
}

func audienceIn(audiences []string) bool {
	for _, a := range audiences {
		if a == PlatformAudience {
			return true
		}
	}
	return false
}

// MembershipsRequest states one person's complete set of groups in a realm,
// as the realm's signed event did: state, not deltas. It reaches the stores
// of the realm's tenant's components that mirror groups, and no other.
type MembershipsRequest struct {
	Tenant  string   `json:"tenant"`
	Realm   string   `json:"realm"`
	Subject string   `json:"subject"`
	Groups  []string `json:"groups"`
}

// GroupDeletedRequest says a group of a realm no longer exists.
type GroupDeletedRequest struct {
	Tenant string `json:"tenant"`
	Realm  string `json:"realm"`
	Group  string `json:"group"`
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
	route("PUT /v1/platform/memberships", s.platformMemberships)
	route("POST /v1/platform/group-deleted", s.platformGroupDeleted)
	route("GET /v1/platform/stores/{tenant}/{component}", s.platformExport)
	route("PUT /v1/platform/stores/{tenant}/{component}", s.platformRestore)
	route("DELETE /v1/platform/stores/{tenant}/{component}", s.platformPurge)
	route("DELETE /v1/platform/tenants/{tenant}", s.platformPurgeTenant)
	return mux
}

// mirrors are the entries a realm's memberships reach: the components of
// that tenant, of that realm, that mirror groups.
func (s *Server) mirrors(tenant, realm string) []Entry {
	var out []Entry
	for _, e := range s.table.Load().byHash {
		if e.Tenant == tenant && e.Realm == realm && e.MirrorGroups {
			out = append(out, e)
		}
	}
	return out
}

func (s *Server) platformMemberships(r *http.Request) (any, error) {
	var req MembershipsRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if !dnsLabel.MatchString(req.Tenant) || req.Subject == "" {
		return nil, badRequest("tenant and subject are required")
	}
	user, err := UserObject(req.Realm, req.Subject)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	want := map[string]bool{}
	for _, g := range req.Groups {
		obj, err := GroupObject(req.Realm, g)
		if err != nil {
			// A nested group, or a name OpenFGA cannot hold: not mirrored,
			// as the kernel's store does not hold one either.
			continue
		}
		want[obj] = true
	}
	prefix := groupType + ":" + req.Realm + "/"
	for _, e := range s.mirrors(req.Tenant, req.Realm) {
		ids, err := s.stores.Resolve(r.Context(), e)
		if err != nil {
			return nil, err
		}
		stored, err := s.fga.readAll(r.Context(), ids.Store, fgaTuple{User: user, Object: groupType + ":"})
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		var deletes []Tuple
		for _, t := range stored {
			if t.Relation != "member" || !strings.HasPrefix(t.Object, prefix) {
				continue
			}
			have[t.Object] = true
			if !want[t.Object] {
				deletes = append(deletes, t)
			}
		}
		var writes []Tuple
		for obj := range want {
			if !have[obj] {
				writes = append(writes, Tuple{User: user, Relation: "member", Object: obj})
			}
		}
		if err := s.fga.writeTuples(r.Context(), ids.Store, ids.Model, writes, deletes); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (s *Server) platformGroupDeleted(r *http.Request) (any, error) {
	var req GroupDeletedRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	obj, err := GroupObject(req.Realm, req.Group)
	if err != nil {
		return nil, nil // a group that was never mirrored has no members here
	}
	for _, e := range s.mirrors(req.Tenant, req.Realm) {
		ids, err := s.stores.Resolve(r.Context(), e)
		if err != nil {
			return nil, err
		}
		members, err := s.fga.readAll(r.Context(), ids.Store, fgaTuple{Relation: "member", Object: obj})
		if err != nil {
			return nil, err
		}
		if err := s.fga.writeTuples(r.Context(), ids.Store, ids.Model, nil, members); err != nil {
			return nil, err
		}
	}
	return nil, nil
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

// errForbiddenGroupTuple is a component writing a tuple of type group.
var errForbiddenGroupTuple = errors.New("tuples whose user or object is a group are the platform's")
