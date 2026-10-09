/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package bouncer is the ext-auth bouncer: L2 of the edge.
//
// The Gateway has established who the caller is (L1: a session or a bearer
// token). This answers one question per request -- may this person reach
// this host at all -- by asking the authorization store the relation the
// route declares, over the stored membership projection, and caching the
// answer per (subject, session, route). It decides reachability; the app
// behind the route decides everything finer.
package bouncer

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
	// AuthMode is the route's L1: "oidc", a session the Gateway's OAuth2
	// filter establishes, or "bearer", a token the caller presents.
	//
	// On an oidc route the OAuth2 filter runs ahead of this service. It
	// answers its own callback and logout paths, sends a request with no
	// session to sign in, refreshes a session whose access token has run out,
	// and only then lets a request through -- carrying the session's current
	// token, which it put there itself after removing whatever the client
	// sent in that place. So every request that arrives here on such a route
	// carries a token this service can verify, and one that does not is
	// refused: there is no "no session yet" to be lenient about.
	AuthMode string `json:"authMode"`
	// KeepClientToken says the Authorization header on this route is the
	// page's own and not the edge's: the gateway does not put the session's
	// access token there and this service neither reads it nor removes it.
	//
	// Not the same as ForwardToken, and conflating them broke the Keycloak
	// console: that page mints a token with its own code flow and calls the
	// Admin REST API with it. Stripping the header is a 401; replacing it
	// with the edge's is "Token issued for an application that is not the
	// admin console". It needs neither -- only to be left alone.
	//
	// The session is then proved by its ID token, which the gateway hands
	// over in HeaderIDToken, and IDTokenAudience says whose it must be.
	KeepClientToken bool `json:"keepClientToken,omitempty"`
	// IDTokenAudience is the zone's client, the one an ID token on a
	// KeepClientToken route must have been issued to. Without it such a route
	// has nothing to hold a token against and refuses everybody.
	IDTokenAudience string `json:"idTokenAudience,omitempty"`
	// ForwardToken keeps the Authorization header for the backend. Only a
	// route whose exposure declares it -- the desktop, which relays to the
	// director -- has it; every other backend gets identity headers instead.
	ForwardToken bool `json:"forwardToken,omitempty"`
	// ExchangeScope makes the backend's Authorization header a token of the
	// person that is valid at this app alone: the session's token exchanged
	// at the realm, asking for this one scope, which names the app as the
	// audience (exchange.go). Empty on every route that did not ask for it.
	ExchangeScope string `json:"exchangeScope,omitempty"`
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
	// SessionCookies and SessionCookiePrefixes name the cookies the Gateway's
	// OAuth2 filter keeps the session in on this route, so they can be taken
	// out of the request before it goes on to the backend.
	//
	// The filter decrypts the token cookies into the request it passes on,
	// so without this the backend would be handed the person's access token
	// and ID token in the Cookie header, whatever ForwardToken says about
	// the Authorization header. SessionCookies are exact names: the two the
	// route's policy gives the token cookies. SessionCookiePrefixes are the
	// cookies the filter names itself, a fixed word followed by a suffix it
	// derives from the policy and, for the two that carry a sign-in in
	// progress, from that sign-in.
	//
	// The operator states both; this service guesses at neither. A route
	// that names none keeps its Cookie header as it came, which is every
	// bearer route: there is no session there.
	SessionCookies        []string `json:"sessionCookies,omitempty"`
	SessionCookiePrefixes []string `json:"sessionCookiePrefixes,omitempty"`
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
	// Checkers are the components that may ask whether a person may use an
	// app of their tenant (check.go). None by default.
	Checkers []Checker `json:"checkers,omitempty"`
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
		if r.ExchangeScope != "" && (r.AuthMode != AuthModeOIDC || r.ForwardToken || r.KeepClientToken) {
			return nil, fmt.Errorf("route table: entry %d (%q) exchanges a token only behind a session, and hands on one token", i, r.Host)
		}
		if seen[host] {
			return nil, fmt.Errorf("route table: host %q listed twice", host)
		}
		seen[host] = true
		t.Routes[i].Host = host
		t.Routes[i].DenyPaths = normalisePaths(r.DenyPaths)
		// An empty name would own nothing and an empty prefix everything;
		// neither is a name the operator wrote on purpose.
		for _, n := range append(append([]string{}, r.SessionCookies...), r.SessionCookiePrefixes...) {
			if strings.TrimSpace(n) == "" {
				return nil, fmt.Errorf("route table: entry %d (%q) names an empty session cookie", i, r.Host)
			}
		}
	}
	if err := validateCheckers(t.Checkers); err != nil {
		return nil, err
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
