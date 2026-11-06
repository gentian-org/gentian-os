/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package tokenreview identifies the caller behind a ServiceAccount token
// projected for one audience: it asks the API server (TokenReview) and keeps
// a positive answer for a short time.
package tokenreview

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TTL is how long a token the API server vouched for is admitted without
// asking again. It bounds two things: the load a busy caller puts on the API
// server, and how long a token outlives the pod it was bound to.
const TTL = 30 * time.Second

// Limit bounds the cache. Only tokens the API server vouched for are kept,
// and a listener admits a handful of workloads, so it is never near this;
// the bound is there so that nothing can grow it without limit.
const Limit = 256

// Timeout bounds one question to the API server. A token that cannot be
// reviewed in this time is not admitted.
const Timeout = 10 * time.Second

// Reviewer asks the API server about a token. It is the one method of
// client-go's TokenReviewInterface, so the clientset's implementation
// satisfies it and a test can stand in for the API server.
type Reviewer interface {
	Create(ctx context.Context, review *authenticationv1.TokenReview, opts metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// ErrNotAuthenticated is the API server not vouching for the token for the
// audience.
var ErrNotAuthenticated = errors.New("the token is not a valid ServiceAccount token for this audience")

// Cache identifies tokens for one audience.
type Cache struct {
	reviews  Reviewer
	audience string

	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	entries map[[sha256.Size]byte]entry
}

// entry is one positive answer and when it stops being believed.
type entry struct {
	username string
	until    time.Time
}

// New returns the check for tokens projected for audience.
func New(reviews Reviewer, audience string) *Cache {
	return &Cache{reviews: reviews, audience: audience, entries: map[[sha256.Size]byte]entry{}}
}

// Ready reports whether a token can be identified at all: there is an API
// server to ask and an audience to ask about.
func (c *Cache) Ready() bool {
	return c != nil && c.reviews != nil && c.audience != ""
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Identify returns the username the API server reports for token.
//
// It answers ErrNotAuthenticated when the API server does not vouch for the
// token for the audience, and any other error when the API server could not
// be asked -- which the caller must treat as a refusal, not as a pass.
//
// The audience is required in the answer, not assumed from the question: an
// authenticator that does not know about audiences reports the token as
// genuine and leaves the list empty, and a token for the API server itself
// must not be taken for one issued for this audience. An answer that says
// authenticated and carries an error is a refusal.
//
// A positive answer is kept for TTL under a hash of the token; the token
// itself is neither stored nor logged. A refusal is never kept.
func (c *Cache) Identify(ctx context.Context, token string) (string, error) {
	if !c.Ready() {
		return "", errors.New("no API server or audience to review a token against")
	}
	key := sha256.Sum256([]byte(token))
	now := c.now()
	c.mu.Lock()
	if hit, ok := c.entries[key]; ok && now.Before(hit.until) {
		c.mu.Unlock()
		return hit.username, nil
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	answer, err := c.reviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{c.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("ask the API server about a token: %w", err)
	}
	if answer == nil {
		return "", errors.New("ask the API server about a token: no answer")
	}
	status := answer.Status
	if !status.Authenticated || status.Error != "" || status.User.Username == "" ||
		!slices.Contains(status.Audiences, c.audience) {
		return "", ErrNotAuthenticated
	}

	c.mu.Lock()
	for k, v := range c.entries {
		if !now.Before(v.until) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= Limit {
		clear(c.entries)
	}
	c.entries[key] = entry{username: status.User.Username, until: now.Add(TTL)}
	c.mu.Unlock()
	return status.User.Username, nil
}
