/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authn

import "slices"

// A delegated token: a person's token a component obtained while the person
// was away, by vouching (requires.services.vouching), made out to the
// platform's API under one of the director scopes. The realm issues it to
// the component's client, vouch-<profile>, which is how it is told apart
// from a token the person signed in for.
//
// Such a token reaches the routes its scopes name and no other, at the
// director and at the usher alike, whatever the person may do: the person's
// own relations are asked as well, so a component never does more than the
// person may, and never more than it declared.

// delegatedClientPrefix is VouchingAlias's prefix (internal/controller).
const delegatedClientPrefix = "vouch-"

// delegatedRoutes is every route a delegated token may call, by its
// registered pattern, with the scopes that open it.
var delegatedRoutes = map[string][]string{
	// The director: the cluster's plans, and moving a tenant to one.
	"GET /v1/clusters/{c}/resource-plans":        {"director-plans-read", "director-plans-apply"},
	"PUT /v1/clusters/{c}/tenants/{t}/resources": {"director-plans-apply"},
	// The usher: every tenant's plan and ceiling, and their billed intervals.
	"GET /v1/clusters/{c}/resources":        {"director-plans-read", "director-plans-apply"},
	"GET /v1/clusters/{c}/resources/report": {"director-plans-read", "director-plans-apply"},
}

// Delegated reports whether the token was obtained by vouching rather than
// by the person signing in.
func (i *Identity) Delegated() bool {
	return i != nil && len(i.Client) > len(delegatedClientPrefix) && i.Client[:len(delegatedClientPrefix)] == delegatedClientPrefix
}

// MayCall reports whether the token may call the route registered as
// pattern. A token the person signed in for may call any; what they may do
// there is the authorization graph's answer.
func (i *Identity) MayCall(pattern string) bool {
	if !i.Delegated() {
		return true
	}
	for _, scope := range delegatedRoutes[pattern] {
		if slices.Contains(i.Scopes, scope) {
			return true
		}
	}
	return false
}
