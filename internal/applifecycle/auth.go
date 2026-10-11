/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Who may call this listener is decided by workload identity, not by a
// string both sides hold.
//
// The director and the usher each present a ServiceAccount token the kubelet
// projected into their pod for one audience, this listener's. The operator
// hands the token to the API server (TokenReview), which says whether it is
// genuine, still valid, meant for this audience, and whose it is. The caller
// is then admitted by that name: the director's ServiceAccount for every
// route, the usher's for the routes registered as reads, nobody else for
// anything.
//
// What that changes against the two shared tokens it replaces: there is no
// Secret to read, copy or leak; a token lives ten minutes and is bound to the
// pod it was issued to, so it dies with the pod; and a token issued for any
// other audience -- the API server's own included -- is refused here.
//
// What it does not change: the listener still verifies no person. The name
// recorded against a command is the one the director passes in a header, and
// the usher's identity still reads every tenant's live state.

// The marker is a free-floating block: controller-gen ignores a block that is
// part of a declaration's doc comment.
//
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create

// reviewCacheTTL is how long a token the API server vouched for is admitted
// without asking again. It bounds two things: the load a busy console puts on
// the API server, and how long a token outlives the pod it was bound to.
const reviewCacheTTL = 30 * time.Second

// reviewCacheLimit bounds the cache. Only tokens the API server vouched for
// are kept, and two workloads present them, so it is never near this; the
// bound is there so that nothing can grow it without limit.
const reviewCacheLimit = 256

// reviewTimeout bounds one question to the API server. A request that cannot
// be decided in this time is refused.
const reviewTimeout = 10 * time.Second

// TokenReviewer asks the API server about a token. It is the one method of
// client-go's TokenReviewInterface, so the clientset's implementation
// satisfies it and a test can stand in for the API server.
type TokenReviewer interface {
	Create(ctx context.Context, review *authenticationv1.TokenReview, opts metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// Callers names the audience a token must be issued for and the two
// identities this listener admits. Each is a full username as the API server
// reports it: system:serviceaccount:<namespace>:<name>.
type Callers struct {
	// Audience is what a caller's token must have been issued for. Empty
	// admits nobody.
	Audience string
	// Director is admitted to every route. Empty means there is no director.
	Director string
	// Reader is admitted to the routes registered with Read. Empty means
	// there is no reader.
	Reader string
}

// ServiceAccountUsername is the name the API server reports for a
// ServiceAccount, or "" when either part is missing.
func ServiceAccountUsername(namespace, name string) string {
	if namespace == "" || name == "" {
		return ""
	}
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, name)
}

// CallersFromEnv reads who is admitted from the operator's environment, as
// the chart renders it: the audience and the two ServiceAccount names are
// chart values, and the namespace is the operator's own.
func CallersFromEnv(namespace string) Callers {
	return Callers{
		Audience: os.Getenv("APP_LIFECYCLE_AUDIENCE"),
		Director: ServiceAccountUsername(namespace, os.Getenv("APP_LIFECYCLE_DIRECTOR_SERVICE_ACCOUNT")),
		Reader:   ServiceAccountUsername(namespace, os.Getenv("APP_LIFECYCLE_USHER_SERVICE_ACCOUNT")),
	}
}

// errNotAuthenticated is the API server saying the token is not one it
// vouches for, for this audience.
var errNotAuthenticated = errors.New("the token is not a valid ServiceAccount token for this listener")

// CallerAuth identifies the caller of a request by its bearer token.
type CallerAuth struct {
	reviews TokenReviewer
	callers Callers
	now     func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]reviewed
}

// reviewed is one positive answer and when it stops being believed.
type reviewed struct {
	username string
	until    time.Time
}

// NewCallerAuth returns the check for a listener that admits callers.
func NewCallerAuth(reviews TokenReviewer, callers Callers) *CallerAuth {
	return &CallerAuth{
		reviews: reviews,
		callers: callers,
		now:     time.Now,
		cache:   map[[sha256.Size]byte]reviewed{},
	}
}

// configured reports whether anybody at all can be admitted. A listener with
// no audience, no way to ask, or no identity to admit refuses every request
// and says why, rather than answering each with a 401 that explains nothing.
func (a *CallerAuth) configured() bool {
	return a != nil && a.reviews != nil && a.callers.Audience != "" &&
		(a.callers.Director != "" || a.callers.Reader != "")
}

// identify returns the username the API server reports for token.
//
// It answers errNotAuthenticated when the API server does not vouch for the
// token for this audience, and any other error when the API server could not
// be asked -- which the caller must treat as a refusal, not as a pass.
//
// A positive answer is kept for reviewCacheTTL under a hash of the token; the
// token itself is neither stored nor logged. A refusal is never kept.
func (a *CallerAuth) identify(ctx context.Context, token string) (string, error) {
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	if hit, ok := a.cache[key]; ok && now.Before(hit.until) {
		a.mu.Unlock()
		return hit.username, nil
	}
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()
	answer, err := a.reviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{a.callers.Audience},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("ask the API server about a caller's token: %w", err)
	}
	if answer == nil {
		return "", errors.New("ask the API server about a caller's token: no answer")
	}
	status := answer.Status
	// The audience is required in the answer, not assumed from the question:
	// an authenticator that does not know about audiences reports the token
	// as genuine and leaves this list empty, and a token for the API server
	// itself must not be taken for one issued for this listener.
	if !status.Authenticated || status.Error != "" || status.User.Username == "" ||
		!slices.Contains(status.Audiences, a.callers.Audience) {
		return "", errNotAuthenticated
	}

	a.mu.Lock()
	for k, v := range a.cache {
		if !now.Before(v.until) {
			delete(a.cache, k)
		}
	}
	if len(a.cache) >= reviewCacheLimit {
		clear(a.cache)
	}
	a.cache[key] = reviewed{username: status.User.Username, until: now.Add(reviewCacheTTL)}
	a.mu.Unlock()
	return status.User.Username, nil
}
