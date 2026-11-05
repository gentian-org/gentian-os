/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usher

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gentian-org/gentian-os/internal/guard/guardtest"
)

// What the usher's guard does with each kind of caller, on each kind of
// route registration, held to testdata/guard.json: the answer, the
// questions put to the store, the log and what the route is handed.
func TestTheUshersGuardOnTheWire(t *testing.T) {
	ids := guardtest.Authn{
		"ada":     {Subject: "ada", Realm: "gentian", Name: "Ada", Email: "ada@example.org", Issuer: "https://id/realms/gentian"},
		"tenant":  {Subject: "u1", Realm: "demo", Name: "Una"},
		"norealm": {Subject: "bob", Name: "Bob", Email: "bob@example.org"},
		"weird":   {Subject: "a b", Realm: "demo"},
	}
	held := map[string]bool{
		"user:ada can_view tenant:demo":           true,
		"user:demo/u1 can_view tenant:demo":       true,
		"user:ada can_configure cluster:c1":       true,
		"user:ada can_install_app tenant:demo":    true,
		"user:bob can_view tenant:demo":           true,
		"user:demo/u1 can_install_app tenant:own": true,
	}
	build := func(checks *guardtest.Checks, logs *guardtest.Logs, handled *string) http.Handler {
		s := &Server{cfg: Config{Authn: ids, Authz: checks, Log: logs.Logger(), Cluster: "c1"}, mux: http.NewServeMux()}
		probe := func(w http.ResponseWriter, _ *http.Request, c call) {
			*handled = fmt.Sprintf("%+v", c)
			w.WriteHeader(http.StatusNoContent)
		}
		s.guarded("GET /probe/tenants/{t}", "can_view", tenantObject, probe)
		s.guarded("GET /probe/clusters/{c}", "can_configure", clusterObject, probe)
		s.guarded("POST /probe/tenants/{t}/actions/go", "can_install_app", tenantObject, probe)
		s.guardedAny("GET /probe/any/{t}", []string{"can_audit", "can_install_app", "can_view"}, tenantObject, probe)
		return s
	}
	const rid = "req-0001-abcd"
	type kase struct {
		name string
		req  guardtest.Request
		err  error
	}
	cases := []kase{
		{"guarded: allowed as the account", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "ada", RequestID: rid}, nil},
		{"guarded: allowed as the realm's holder", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "tenant", RequestID: rid}, nil},
		{"guarded: allowed without a realm", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "norealm", RequestID: rid}, nil},
		{"guarded: refused", guardtest.Request{Method: "GET", Path: "/probe/tenants/other", Token: "tenant", RequestID: rid}, nil},
		{"guarded: no bearer", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", RequestID: rid}, nil},
		{"guarded: not a bearer", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Authorization: "Basic YWRhOmFkYQ==", RequestID: rid}, nil},
		{"guarded: unknown token", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "nobody", RequestID: rid}, nil},
		{"guarded: a subject OpenFGA cannot hold", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "weird", RequestID: rid}, nil},
		{"guarded: a tenant name of 63", guardtest.Request{Method: "GET", Path: "/probe/tenants/a23456789012345678901234567890123456789012345678901234567890123", Token: "ada", RequestID: rid}, nil},
		{"guarded: an invalid tenant", guardtest.Request{Method: "GET", Path: "/probe/tenants/Not_A_Name", Token: "ada", RequestID: rid}, nil},
		{"guarded: the store fails", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "ada", RequestID: rid}, errors.New("down")},
		{"guarded: a minted id", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "ada"}, nil},
		{"guarded: an id that is not kept", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "ada", RequestID: "short"}, nil},
		{"guarded: an unknown token with a minted id", guardtest.Request{Method: "GET", Path: "/probe/tenants/demo", Token: "nobody"}, nil},
		{"cluster: allowed", guardtest.Request{Method: "GET", Path: "/probe/clusters/c1", Token: "ada", RequestID: rid}, nil},
		{"cluster: another cluster", guardtest.Request{Method: "GET", Path: "/probe/clusters/c2", Token: "ada", RequestID: rid}, nil},
		{"cluster: refused", guardtest.Request{Method: "GET", Path: "/probe/clusters/c1", Token: "tenant", RequestID: rid}, nil},
		{"action: allowed", guardtest.Request{Method: "POST", Path: "/probe/tenants/demo/actions/go", Token: "ada", RequestID: rid}, nil},
		{"action: refused", guardtest.Request{Method: "POST", Path: "/probe/tenants/demo/actions/go", Token: "tenant", RequestID: rid}, nil},
		{"action: the store fails", guardtest.Request{Method: "POST", Path: "/probe/tenants/demo/actions/go", Token: "tenant", RequestID: rid}, errors.New("down")},
		{"any: the second relation", guardtest.Request{Method: "GET", Path: "/probe/any/demo", Token: "ada", RequestID: rid}, nil},
		{"any: as the holder", guardtest.Request{Method: "GET", Path: "/probe/any/demo", Token: "tenant", RequestID: rid}, nil},
		{"any: none held", guardtest.Request{Method: "GET", Path: "/probe/any/other", Token: "ada", RequestID: rid}, nil},
		{"any: the store fails", guardtest.Request{Method: "GET", Path: "/probe/any/demo", Token: "ada", RequestID: rid}, errors.New("down")},
		{"any: a tenant name of 53", guardtest.Request{Method: "GET", Path: "/probe/any/a2345678901234567890123456789012345678901234567890123", Token: "ada", RequestID: rid}, nil},
	}
	var got []guardtest.Case
	for _, c := range cases {
		checks := &guardtest.Checks{Held: held, Err: c.err}
		logs := &guardtest.Logs{}
		var handled string
		h := build(checks, logs, &handled)
		got = append(got, guardtest.Case{Name: c.name, Exchange: guardtest.Do(t, h, c.req, checks, logs, &handled)})
	}
	guardtest.Golden(t, "guard", got)
}
