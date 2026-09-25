/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package authz is the ext-auth shim: L2 of the edge (networking.md §2).
//
// The Gateway has established who the caller is (L1: a session or a bearer
// token). This answers one question per request -- may this person reach
// this host at all -- by asking the authorization store the relation the
// route declares, over the stored membership projection, and caching the
// answer per (subject, session, route). It decides reachability; the app
// behind the route decides everything finer.
package authz

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Route is one entry of the table the operator writes beside the routes it
// reconciles: the host, and what must hold for a caller to reach it.
type Route struct {
	// Host is the hostname the route serves, exactly.
	Host string `json:"host"`
	// Relation and Object are the store question: user:<sub> Relation Object.
	Relation string `json:"relation"`
	Object   string `json:"object"`
	// AuthMode is the route's L1: "oidc", a session the Gateway's OIDC filter
	// establishes, or "bearer", a token the caller presents. Envoy Gateway
	// runs ext_authz before its OIDC filter, so on an oidc route a request
	// with no valid token is not this shim's to refuse: it passes, carrying
	// no identity, and the OIDC filter behind sends it to sign in. On a
	// bearer route the same request is refused here.
	AuthMode string `json:"authMode"`
	// AccessTokenCookie is where the zone's session keeps the access token,
	// for routes whose token is not forwarded as a bearer.
	AccessTokenCookie string `json:"accessTokenCookie,omitempty"`
	// IDTokenCookie is where the zone's session keeps the ID token.
	//
	// Only sign-out needs it. Keycloak shows a "did you really mean it" page
	// for any logout that arrives without an id_token_hint, because a logout
	// it cannot attribute to a session might have been triggered by a link on
	// somebody else's page. The hint is sitting in this cookie, so the edge
	// can answer that question on the person's behalf and they never see the
	// page.
	IDTokenCookie string `json:"idTokenCookie,omitempty"`
	// EndSessionURL is the realm's OIDC end_session_endpoint.
	//
	// Written by the operator, which knows the issuer and the realm, rather
	// than discovered here: this service must not depend on reaching the
	// identity provider to answer a request, and a sign-out that waited on
	// discovery would fail exactly when the identity provider is the thing
	// that is unwell. Empty means sign-out falls back to clearing the edge's
	// own cookies and nothing else.
	EndSessionURL string `json:"endSessionURL,omitempty"`
	// KeepClientToken leaves the caller's own Authorization header alone
	// without the edge putting its token there.
	//
	// Not the same as ForwardToken, and conflating them broke the Keycloak
	// console: that page mints a token with its own code flow and calls the
	// Admin REST API with it. Stripping the header is a 401; replacing it
	// with the edge's is "Token issued for an application that is not the
	// admin console". It needs neither -- only to be left alone.
	KeepClientToken bool `json:"keepClientToken,omitempty"`
	// ForwardToken keeps the Authorization header for the backend. Only a
	// route whose exposure declares it -- the desktop, which relays to the
	// director -- has it; every other backend gets identity headers instead.
	ForwardToken bool `json:"forwardToken,omitempty"`
	// DenyPaths are refused before anything else is asked, whoever is
	// calling. It is what lets a component publish a UI without publishing
	// its own administrative endpoints, and the alternative is every app
	// carrying that logic itself.
	//
	// Enforced here rather than by leaving the path unrouted, because a
	// gateway route is a prefix and the more specific rule would still need
	// somewhere to send the request. Refusing at L2 is the one place that
	// already sees every request to the host.
	//
	// Unioned across a host's exposures: an entry that denies a path denies
	// it for the host, because deny wins.
	DenyPaths []string `json:"denyPaths,omitempty"`
}

// Denies reports whether a path is one the route refuses outright.
//
// Prefix semantics are the Gateway API's, on segment boundaries: "/admin"
// denies "/admin" and "/admin/users" and does not deny "/administrators".
// Matching on the raw string instead would refuse paths nobody meant to name,
// which for a deny rule is a silent outage rather than a silent hole -- but
// still not what the profile said.
func (r *Route) Denies(path string) bool {
	if r == nil || len(r.DenyPaths) == 0 {
		return false
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	for _, d := range r.DenyPaths {
		if d == "" {
			continue
		}
		if d == "/" {
			return true
		}
		d = strings.TrimSuffix(d, "/")
		if path == d || strings.HasPrefix(path, d+"/") {
			return true
		}
	}
	return false
}

// The two L1 modes a route with an L2 question can have.
const (
	AuthModeOIDC   = "oidc"
	AuthModeBearer = "bearer"
)

// Table is the route table.
type Table struct {
	Routes []Route `json:"routes"`
}

// LoadTable reads a table from a file the operator's ConfigMap is mounted at.
func LoadTable(path string) (*Table, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTable(b)
}

// ParseTable parses a table and refuses one that names a route incompletely:
// a host with no relation is a route nothing decides, which is not a route.
func ParseTable(b []byte) (*Table, error) {
	var t Table
	if err := yaml.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("route table: %w", err)
	}
	seen := map[string]bool{}
	for i, r := range t.Routes {
		host := strings.ToLower(strings.TrimSpace(r.Host))
		if host == "" || r.Relation == "" || r.Object == "" {
			return nil, fmt.Errorf("route table: entry %d (%q) needs host, relation and object", i, r.Host)
		}
		if r.AuthMode != AuthModeOIDC && r.AuthMode != AuthModeBearer {
			return nil, fmt.Errorf("route table: entry %d (%q) needs authMode oidc or bearer", i, r.Host)
		}
		if seen[host] {
			return nil, fmt.Errorf("route table: host %q listed twice", host)
		}
		seen[host] = true
		t.Routes[i].Host = host
		t.Routes[i].DenyPaths = normalisePaths(r.DenyPaths)
	}
	return &t, nil
}

// Match returns the route for a host, or nil. A host with no route is not
// routable: nothing is reachable without a class.
func (t *Table) Match(host string) *Route {
	if t == nil {
		return nil
	}
	host = strings.ToLower(host)
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	for i := range t.Routes {
		if t.Routes[i].Host == host {
			return &t.Routes[i]
		}
	}
	return nil
}

// normalisePaths trims each entry and gives it a leading slash, so a profile
// that wrote "admin" denies the same thing one that wrote "/admin" does.
// Empty entries are dropped rather than treated as "/", which would refuse
// the whole host on a typo.
func normalisePaths(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
