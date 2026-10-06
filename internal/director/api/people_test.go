/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"net/http"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// The director serves nothing about people.
//
// People, groups and the realm's settings were routes here while the director
// held a Keycloak credential for every realm. They are the registrar's now
// (internal/registrar), with the credential. This is every path that moved,
// asked of a director with everything else it can be given, by the people who
// were allowed each one: the answer is that the route does not exist, and the
// store is not even asked.
func TestTheDirectorServesNothingAboutPeople(t *testing.T) {
	h := startWith(t, &liveTenants{policy: map[string]string{}})
	c := "/v1/clusters/" + dt.Cluster
	h.asked.reset()
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/v1/tenants/demo/people", ""},
		{"GET", "/v1/tenants/demo/people/u1", ""},
		{"GET", "/v1/tenants/demo/groups", ""},
		{"GET", "/v1/tenants/demo/identity", ""},
		{"GET", "/v1/tenants/demo/group-members?group=gentian:tenant:demo:members", ""},
		{"GET", "/v1/tenants/demo/templates", ""},
		{"POST", "/v1/tenants/demo/actions/invite-person", `{"email":"ada@example.com"}`},
		{"POST", "/v1/tenants/demo/actions/set-membership", `{"person":"u1","group":"g","member":true}`},
		{"POST", "/v1/tenants/demo/actions/send-password-reset", `{"person":"u1"}`},
		{"POST", "/v1/tenants/demo/actions/set-password-policy", `{"passwordPolicy":""}`},
		{"POST", "/v1/tenants/demo/actions/update-person", `{"person":"u1"}`},
		{"POST", "/v1/tenants/demo/actions/remove-person", `{"person":"u1"}`},
		{"POST", "/v1/tenants/demo/actions/require-totp", `{"person":"u1"}`},
		{"POST", "/v1/tenants/demo/actions/remove-totp", `{"person":"u1"}`},
		{"POST", "/v1/tenants/demo/actions/create-group", `{"name":"sales"}`},
		{"POST", "/v1/tenants/demo/actions/delete-group", `{"group":"g"}`},
		{"POST", "/v1/tenants/demo/actions/rename-group", `{"group":"g","name":"n"}`},
		{"POST", c + "/tenants/demo/actions/activate-admin", ""},
		{"GET", c + "/people/count", ""},
	} {
		for _, who := range [][2]string{{"tenant-demo", "tom"}, {"gentian", "alice"}} {
			status, _ := h.do(t, rt.method, rt.path, h.token(t, who[0], who[1]), rt.body)
			if status != http.StatusNotFound {
				t.Errorf("%s %s answered %s %d; the director must not serve it", rt.method, rt.path, who[1], status)
			}
		}
	}
	if q := h.asked.questions(); len(q) != 0 {
		t.Errorf("a route that does not exist asked the store: %v", q)
	}
}
