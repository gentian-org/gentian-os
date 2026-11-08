/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authn

import "testing"

func TestADelegatedTokenReachesOnlyTheRoutesItsScopesName(t *testing.T) {
	signedIn := &Identity{Client: "gentian-edge-platform", Scopes: []string{"openid"}}
	read := &Identity{Client: "vouch-ops-sync", Scopes: []string{"director-plans-read"}}
	apply := &Identity{Client: "vouch-ops-sync", Scopes: []string{"director-plans-apply"}}
	none := &Identity{Client: "vouch-ops-sync", Scopes: []string{"app-wiki"}}
	for _, c := range []struct {
		who     *Identity
		pattern string
		may     bool
	}{
		{signedIn, "DELETE /v1/clusters/{c}/tenants/{t}", true},
		{read, "GET /v1/clusters/{c}/resources/report", true},
		{read, "GET /v1/clusters/{c}/resource-plans", true},
		{read, "PUT /v1/clusters/{c}/tenants/{t}/resources", false},
		{apply, "PUT /v1/clusters/{c}/tenants/{t}/resources", true},
		{apply, "GET /v1/clusters/{c}/resources", true},
		{apply, "PUT /v1/clusters/{c}/resource-plans/{name}", false},
		{apply, "GET /v1/tenants/{t}/resources", false},
		{none, "GET /v1/clusters/{c}/resources", false},
	} {
		if got := c.who.MayCall(c.pattern); got != c.may {
			t.Errorf("%s %v on %q: %v, want %v", c.who.Client, c.who.Scopes, c.pattern, got, c.may)
		}
	}
	if (&Identity{Client: "vouch-"}).Delegated() || !(&Identity{Client: "vouch-x"}).Delegated() {
		t.Error("vouch- alone is no component's client")
	}
}
