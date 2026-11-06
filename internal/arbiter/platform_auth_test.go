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
	"errors"
	"net/http"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// countingReviews answers from a table of token -> status, counts how often
// it was asked, and fails every question while err is set.
type countingReviews struct {
	answers map[string]authenticationv1.TokenReviewStatus
	asked   int
	err     error
}

func (c *countingReviews) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	c.asked++
	if c.err != nil {
		return nil, c.err
	}
	out := review.DeepCopy()
	out.Status = c.answers[review.Spec.Token]
	return out, nil
}

// What the platform path answers for each kind of token, and how often it
// asks the API server when the same token is presented twice.
func TestPlatformAdmission(t *testing.T) {
	ours := []string{PlatformAudience}
	answers := map[string]authenticationv1.TokenReviewStatus{
		"operator": {Authenticated: true, Audiences: ours, User: authenticationv1.UserInfo{Username: operatorSA}},
		"app":      {Authenticated: true, Audiences: ours, User: authenticationv1.UserInfo{Username: "system:serviceaccount:tenant-acme:ledger"}},
		"other-audience": {Authenticated: true, Audiences: []string{"https://kubernetes.default.svc"},
			User: authenticationv1.UserInfo{Username: operatorSA}},
		"no-audience":   {Authenticated: true, User: authenticationv1.UserInfo{Username: operatorSA}},
		"contradictory": {Authenticated: true, Error: "token expired", Audiences: ours, User: authenticationv1.UserInfo{Username: operatorSA}},
		"no-username":   {Authenticated: true, Audiences: ours},
		"refused":       {Authenticated: false, Error: "invalid bearer token"},
	}
	for _, tc := range []struct {
		header     string
		want       int
		wantAsked  int
		apiFailure bool
	}{
		{header: "Bearer operator", want: 0, wantAsked: 1},
		{header: "Bearer app", want: http.StatusForbidden, wantAsked: 1},
		{header: "Bearer other-audience", want: http.StatusUnauthorized, wantAsked: 2},
		{header: "Bearer no-audience", want: http.StatusUnauthorized, wantAsked: 2},
		{header: "Bearer contradictory", want: http.StatusUnauthorized, wantAsked: 2},
		{header: "Bearer no-username", want: http.StatusUnauthorized, wantAsked: 2},
		{header: "Bearer refused", want: http.StatusUnauthorized, wantAsked: 2},
		{header: "Bearer operator", want: http.StatusServiceUnavailable, wantAsked: 2, apiFailure: true},
		{header: "", want: http.StatusUnauthorized, wantAsked: 0},
		{header: "Basic operator", want: http.StatusUnauthorized, wantAsked: 0},
		{header: "Bearer ", want: http.StatusUnauthorized, wantAsked: 0},
	} {
		reviews := &countingReviews{answers: answers}
		if tc.apiFailure {
			reviews.err = errors.New("connection refused")
		}
		auth := &PlatformAuth{Reviews: reviews, Caller: operatorSA}
		for range 2 {
			got := 0
			if ref := auth.admit(context.Background(), tc.header); ref != nil {
				got = ref.status
			}
			if got != tc.want {
				t.Errorf("%q (api failing: %v): %d, want %d", tc.header, tc.apiFailure, got, tc.want)
			}
		}
		if reviews.asked != tc.wantAsked {
			t.Errorf("%q (api failing: %v): the API server was asked %d times for two requests, want %d",
				tc.header, tc.apiFailure, reviews.asked, tc.wantAsked)
		}
	}
}

// Nobody is admitted without a reviewer or a caller to admit.
func TestPlatformAdmissionUnconfigured(t *testing.T) {
	reviews := &countingReviews{answers: map[string]authenticationv1.TokenReviewStatus{
		"operator": {Authenticated: true, Audiences: []string{PlatformAudience}, User: authenticationv1.UserInfo{Username: operatorSA}},
	}}
	for name, auth := range map[string]*PlatformAuth{
		"nil":         nil,
		"no reviewer": {Caller: operatorSA},
		"no caller":   {Reviews: reviews},
	} {
		if ref := auth.admit(context.Background(), "Bearer operator"); ref == nil || ref.status != http.StatusUnauthorized {
			t.Errorf("%s: %v, want a 401", name, ref)
		}
	}
}
