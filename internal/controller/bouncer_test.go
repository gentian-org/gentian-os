/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"
	"testing"
)

// The SecurityPolicy is the whole of L1 and L2 for a kernel-zone route: the
// zone's session and the bouncer, failing closed. What a reviewer has to be able
// to find is here: which client, whose cookie, whether the token is handed on.
func TestKernelSecurityPolicyIsTheZoneSessionAndTheBouncer(t *testing.T) {
	spec := kernelSecurityPolicySpec("k.example", "kernel", "kernel-argocd",
		routeAuthz{relation: "can_configure", object: "cluster:c1"}, "gentian-os-bouncer")
	oidc := spec["oidc"].(map[string]interface{})
	if oidc["clientID"] != edgeKernelClientID {
		t.Fatalf("clientID = %v", oidc["clientID"])
	}
	if oidc["provider"].(map[string]interface{})["issuer"] != "https://id.k.example/auth/realms/kernel" {
		t.Fatalf("issuer = %v", oidc["provider"])
	}
	// No cookieDomain: the session cookie is the host's, not the zone's. A
	// zone-scoped cookie was sent by the browser to every application beside
	// this one, so a single careless application saw a credential good for
	// all of them.
	if _, widened := oidc["cookieDomain"]; widened {
		t.Fatalf("cookieDomain = %v: the session must not be shared across the zone's hosts", oidc["cookieDomain"])
	}
	if oidc["forwardAccessToken"] != false {
		t.Fatalf("forwardAccessToken = %v", oidc["forwardAccessToken"])
	}
	ext := spec["extAuth"].(map[string]interface{})
	if ext["failOpen"] != false {
		t.Fatal("the bouncer must fail closed")
	}
	target := spec["targetRefs"].([]interface{})[0].(map[string]interface{})
	if target["kind"] != "HTTPRoute" || target["name"] != "kernel-argocd" {
		t.Fatalf("target = %v", target)
	}
	// The desktop's route is the one that keeps the token.
	desktop := kernelSecurityPolicySpec("k.example", "kernel", "console", routeAuthz{relation: "can_enter", object: "tenant:platform", forwardToken: true}, "s")
	if desktop["oidc"].(map[string]interface{})["forwardAccessToken"] != true {
		t.Fatal("forwardToken must reach the policy")
	}
}

func TestTheRouteTableListsEveryRouteWithAQuestionSortedByHost(t *testing.T) {
	table, err := bouncerRouteTable([]kernelHTTPRouteSpec{
		{name: "b", host: "headlamp.k.example", authz: &routeAuthz{relation: "can_audit", object: "cluster:c1"}},
		{name: "id", host: "id.k.example"},
		{name: "a", host: "argocd.k.example", authz: &routeAuthz{relation: "can_configure", object: "cluster:c1"}},
	}, []bouncerRoute{{Host: "console.k.example", Relation: "can_enter", Object: "tenant:platform", AccessTokenCookie: edgeKernelAccessTokenCookie, ForwardToken: true, AuthMode: "oidc"}}, "k.example", "kernel")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(table, "argocd.k.example") > strings.Index(table, "headlamp.k.example") {
		t.Fatalf("not sorted by host:\n%s", table)
	}
	// As a host entry, specifically. The identity hostname also appears
	// inside every route's endSessionURL, which is the realm's logout
	// endpoint and has nothing to do with whether id.k.example is routed.
	if strings.Contains(table, "host: id.k.example") {
		t.Fatalf("a route with no question is not in the table:\n%s", table)
	}
	if !strings.Contains(table, "host: console.k.example") {
		t.Fatalf("a component's route is in the table beside the kernel's:\n%s", table)
	}
	if !strings.Contains(table, "authMode: oidc") {
		t.Fatalf("the route's L1 mode is what tells the bouncer whose question a missing session is:\n%s", table)
	}
	if !strings.Contains(table, "accessTokenCookie: "+edgeKernelAccessTokenCookie) {
		t.Fatalf("the zone's cookie name is what the bouncer reads the token from:\n%s", table)
	}
}
