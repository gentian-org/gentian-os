/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package tokenreview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const audience = "listener"

// fake answers from a table of token -> status; a token not in it is
// refused, "nil-answer" gets no answer at all, and err fails every question.
type fake struct {
	answers map[string]authenticationv1.TokenReviewStatus
	asked   int
	err     error
}

func (f *fake) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	if review.Spec.Token == "nil-answer" {
		return nil, nil
	}
	out := review.DeepCopy()
	out.Status = f.answers[review.Spec.Token]
	return out, nil
}

func vouched(user string) authenticationv1.TokenReviewStatus {
	return authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{audience},
		User: authenticationv1.UserInfo{Username: user}}
}

func TestIdentify(t *testing.T) {
	contradictory := vouched("sa")
	contradictory.Error = "token expired"
	noAudience := vouched("sa")
	noAudience.Audiences = nil
	f := &fake{answers: map[string]authenticationv1.TokenReviewStatus{
		"good":          vouched("sa"),
		"contradictory": contradictory,
		"no-audience":   noAudience,
		"no-username":   vouched(""),
	}}
	c := New(f, audience)
	if got, err := c.Identify(context.Background(), "good"); err != nil || got != "sa" {
		t.Fatalf("good: %q %v", got, err)
	}
	for _, token := range []string{"contradictory", "no-audience", "no-username", "unknown"} {
		if _, err := c.Identify(context.Background(), token); !errors.Is(err, ErrNotAuthenticated) {
			t.Errorf("%s: %v, want ErrNotAuthenticated", token, err)
		}
	}
	if _, err := c.Identify(context.Background(), "nil-answer"); err == nil || errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("nil answer: %v, want an error that is not a refusal", err)
	}
	f.err = errors.New("connection refused")
	if _, err := c.Identify(context.Background(), "other"); err == nil || errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("API server away: %v, want an error that is not a refusal", err)
	}
	if New(nil, audience).Ready() || New(f, "").Ready() || (*Cache)(nil).Ready() {
		t.Error("a cache with no reviewer or no audience is ready")
	}
	if _, err := New(nil, audience).Identify(context.Background(), "good"); err == nil {
		t.Error("a cache with no reviewer identified a token")
	}
}

// A positive answer is kept for TTL and then asked for again; a refusal is
// never kept.
func TestCaching(t *testing.T) {
	f := &fake{answers: map[string]authenticationv1.TokenReviewStatus{"good": vouched("sa")}}
	c := New(f, audience)
	now := time.Unix(1_700_000_000, 0)
	c.Now = func() time.Time { return now }
	for range 3 {
		_, _ = c.Identify(context.Background(), "good")
		_, _ = c.Identify(context.Background(), "bad")
	}
	if f.asked != 4 {
		t.Fatalf("asked %d times, want 4 (one for the kept answer, three for the refusal)", f.asked)
	}
	now = now.Add(TTL)
	_, _ = c.Identify(context.Background(), "good")
	if f.asked != 5 {
		t.Fatalf("a kept answer outlived TTL: asked %d times, want 5", f.asked)
	}
	if TTL > time.Minute {
		t.Fatalf("a positive answer is kept for %s; a minute is the most", TTL)
	}
}

// Expired entries are dropped when a new one is kept, and the cache never
// holds more than Limit.
func TestBounded(t *testing.T) {
	f := &fake{answers: map[string]authenticationv1.TokenReviewStatus{}}
	for i := range Limit + 10 {
		f.answers[fmt.Sprint(i)] = vouched(fmt.Sprint("sa-", i))
	}
	c := New(f, audience)
	now := time.Unix(1_700_000_000, 0)
	c.Now = func() time.Time { return now }
	for i := range Limit + 10 {
		_, _ = c.Identify(context.Background(), fmt.Sprint(i))
		if len(c.entries) > Limit {
			t.Fatalf("cache holds %d entries, more than %d", len(c.entries), Limit)
		}
	}
	now = now.Add(TTL)
	_, _ = c.Identify(context.Background(), "0")
	if len(c.entries) != 1 {
		t.Fatalf("expired entries kept: %d entries, want 1", len(c.entries))
	}
}

// The cache is keyed by a hash. The token is not in it.
func TestTheTokenIsNotKept(t *testing.T) {
	f := &fake{answers: map[string]authenticationv1.TokenReviewStatus{"the-token": vouched("sa")}}
	c := New(f, audience)
	if _, err := c.Identify(context.Background(), "the-token"); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 1 {
		t.Fatalf("cache holds %d entries, want 1", len(c.entries))
	}
	for key, kept := range c.entries {
		if strings.Contains(string(key[:]), "the-token") || kept.username != "sa" {
			t.Fatalf("the cache holds the token, or the wrong name: %q", kept.username)
		}
	}
}
