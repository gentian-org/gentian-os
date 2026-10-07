/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"github.com/gentian-org/gentian-os/internal/bouncer"
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
	// The session's access token is put in the Authorization header for the
	// bouncer, on every ordinary route: forwardAccessToken is also what makes
	// the gateway's filter remove a bearer the client sent, so a client
	// cannot present its own in place of the session's.
	if oidc["forwardAccessToken"] != true {
		t.Fatalf("forwardAccessToken = %v: the bouncer would be shown the client's header, or none", oidc["forwardAccessToken"])
	}
	if _, has := oidc["forwardIDToken"]; has {
		t.Fatalf("forwardIDToken = %v on a route whose Authorization header is the edge's", oidc["forwardIDToken"])
	}
	if oidc["logoutPath"] != "/oauth2/logout" {
		t.Fatalf("logoutPath = %v", oidc["logoutPath"])
	}
	if got := oidc["cookieConfig"].(map[string]interface{})["sameSite"]; got != "Lax" {
		t.Fatalf("cookieConfig.sameSite = %v", got)
	}
	if oidc["refreshToken"] != true {
		t.Fatalf("refreshToken = %v", oidc["refreshToken"])
	}
	// What must not be there. Each of these lets a request past the session
	// filter without a session, or hands the token cookies to whoever can
	// read a request; the bouncer refuses what they would let through, and
	// the policy must not ask for it in the first place.
	for _, field := range []string{"passThroughAuthHeader", "denyRedirect", "disableTokenEncryption", "cookieDomain"} {
		if v, has := oidc[field]; has {
			t.Fatalf("%s = %v must not be set on a zone policy", field, v)
		}
	}
	if _, has := spec["jwt"]; has {
		t.Fatal("a zone policy carries no second way to authenticate")
	}
	ext := spec["extAuth"].(map[string]interface{})
	if ext["failOpen"] != false {
		t.Fatal("the bouncer must fail closed")
	}
	target := spec["targetRefs"].([]interface{})[0].(map[string]interface{})
	if target["kind"] != "HTTPRoute" || target["name"] != "kernel-argocd" {
		t.Fatalf("target = %v", target)
	}
	// forwardToken is no longer the policy's: the token is always there for
	// the bouncer, and the bouncer's table says whether the backend gets it.
	desktop := kernelSecurityPolicySpec("k.example", "kernel", "console", routeAuthz{relation: "can_enter", object: "tenant:platform", forwardToken: true}, "s")
	if desktop["oidc"].(map[string]interface{})["forwardAccessToken"] != true {
		t.Fatal("forwardAccessToken must be set on a forwarded route too")
	}
}

// A route that keeps the caller's own Authorization header cannot show the
// bouncer the session there. The gateway hands over the session's ID token in
// a header of its own, and leaves the Authorization header alone.
func TestARouteThatKeepsTheCallersTokenShowsTheBouncerTheIDToken(t *testing.T) {
	spec := kernelSecurityPolicySpec("k.example", "kernel", kernelRouteKeycloakAdmin,
		routeAuthz{relation: "can_configure", object: "cluster:c1", keepClientToken: true}, "gentian-os-bouncer")
	oidc := spec["oidc"].(map[string]interface{})
	if oidc["forwardAccessToken"] != false {
		t.Fatalf("forwardAccessToken = %v: the gateway would replace the console's own bearer", oidc["forwardAccessToken"])
	}
	header := oidc["forwardIDToken"].(map[string]interface{})["header"]
	if header != bouncer.HeaderIDToken {
		t.Fatalf("forwardIDToken.header = %v, and the bouncer reads %s", header, bouncer.HeaderIDToken)
	}
	if strings.EqualFold(header.(string), "authorization") {
		t.Fatal("the ID token must not be put in the header the route keeps")
	}
	if spec["extAuth"].(map[string]interface{})["failOpen"] != false {
		t.Fatal("the bouncer must fail closed")
	}
	// The path the operator and the bouncer both name for signing out.
	if edgeLogoutPath != bouncer.LogoutPath || oidc["logoutPath"] != bouncer.LogoutPath {
		t.Fatalf("logout path: operator %q, policy %v, bouncer %q", edgeLogoutPath, oidc["logoutPath"], bouncer.LogoutPath)
	}
}

func TestTheRouteTableListsEveryRouteWithAQuestionSortedByHost(t *testing.T) {
	table, err := bouncerRouteTable([]kernelHTTPRouteSpec{
		{name: "b", host: "headlamp.k.example", authz: &routeAuthz{relation: "can_audit", object: "cluster:c1"}},
		{name: "id", host: "id.k.example"},
		{name: "a", host: "argocd.k.example", authz: &routeAuthz{relation: "can_configure", object: "cluster:c1"}},
	}, []bouncerRoute{{Host: "console.k.example", Relation: "can_enter", Object: "tenant:platform", ForwardToken: true, AuthMode: "oidc"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(table, "argocd.k.example") > strings.Index(table, "headlamp.k.example") {
		t.Fatalf("not sorted by host:\n%s", table)
	}
	if strings.Contains(table, "host: id.k.example") {
		t.Fatalf("a route with no question is not in the table:\n%s", table)
	}
	if !strings.Contains(table, "host: console.k.example") {
		t.Fatalf("a component's route is in the table beside the kernel's:\n%s", table)
	}
	if !strings.Contains(table, "authMode: oidc") {
		t.Fatalf("the route's L1 mode is what tells the bouncer whose question a missing session is:\n%s", table)
	}
	// Nothing about cookies: the bouncer does not read them, and nothing
	// about where a session ends: the gateway's logout does that.
	for _, gone := range []string{"Cookie", "endSessionURL"} {
		if strings.Contains(table, gone) {
			t.Fatalf("the table still carries %s:\n%s", gone, table)
		}
	}
	// And the bouncer reads exactly what was written.
	parsed, err := bouncer.ParseTable([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	if r := parsed.Match("console.k.example"); r == nil || !r.ForwardToken || r.AuthMode != bouncer.AuthModeOIDC {
		t.Fatalf("the bouncer's reading of the desktop route: %+v", r)
	}
}
