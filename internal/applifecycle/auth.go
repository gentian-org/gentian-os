/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"fmt"
	"os"

	"github.com/gentian-org/gentian-os/internal/tokenreview"
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

// CallerAuth identifies the caller of a request by its bearer token.
type CallerAuth struct {
	review  *tokenreview.Cache
	callers Callers
}

// NewCallerAuth returns the check for a listener that admits callers.
func NewCallerAuth(reviews tokenreview.Reviewer, callers Callers) *CallerAuth {
	return &CallerAuth{review: tokenreview.New(reviews, callers.Audience), callers: callers}
}

// configured reports whether anybody at all can be admitted. A listener with
// no audience, no way to ask, or no identity to admit refuses every request
// and says why, rather than answering each with a 401 that explains nothing.
func (a *CallerAuth) configured() bool {
	return a != nil && a.review.Ready() && (a.callers.Director != "" || a.callers.Reader != "")
}
