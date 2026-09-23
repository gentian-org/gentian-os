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
	// AccessTokenCookie is where the zone's session keeps the access token,
	// for routes whose token is not forwarded as a bearer.
	AccessTokenCookie string `json:"accessTokenCookie,omitempty"`
	// ForwardToken keeps the Authorization header for the backend. Only a
	// route whose exposure declares it -- the desktop, which relays to the
	// director -- has it; every other backend gets identity headers instead.
	ForwardToken bool `json:"forwardToken,omitempty"`
}

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
		if seen[host] {
			return nil, fmt.Errorf("route table: host %q listed twice", host)
		}
		seen[host] = true
		t.Routes[i].Host = host
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
