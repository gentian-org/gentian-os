/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// rightsStore is the rights store in memory, as Read and Write answer.
type rightsStore struct {
	tuples map[authz.Tuple]bool
	fail   error
}

func (s *rightsStore) Read(_ context.Context, f authz.Tuple) ([]authz.Tuple, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	var out []authz.Tuple
	for t := range s.tuples {
		if (f.User == "" || f.User == t.User) && (f.Relation == "" || f.Relation == t.Relation) &&
			(f.Object == "" || f.Object == t.Object || (strings.HasSuffix(f.Object, ":") && strings.HasPrefix(t.Object, f.Object))) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *rightsStore) Write(_ context.Context, writes, deletes []authz.Tuple) error {
	if s.fail != nil {
		return s.fail
	}
	for _, t := range deletes {
		delete(s.tuples, t)
	}
	for _, t := range writes {
		s.tuples[t] = true
	}
	return nil
}

// demoRights is tenant demo as the projection leaves it: attached, with the
// position Tenant administrator and the position User, and an app.
func demoRights() *rightsStore {
	admins := authz.Position("demo", authz.PositionTenantAdministrator)
	users := authz.Position("demo", authz.PositionUser)
	return &rightsStore{tuples: map[authz.Tuple]bool{
		{User: "cluster:c1", Relation: "cluster", Object: "tenant:demo"}:             true,
		{User: "tenant:demo", Relation: "tenant", Object: admins}:                    true,
		{User: "tenant:demo", Relation: "tenant", Object: users}:                     true,
		{User: authz.Holders(admins), Relation: "admin", Object: "tenant:demo"}:      true,
		{User: authz.Holders(users), Relation: "user", Object: "tenant:demo"}:        true,
		{User: "tenant:demo", Relation: "tenant", Object: authz.App("demo", "wiki")}: true,
	}}
}

func askAssignments(mux http.Handler, method, path, token, body string) (int, map[string]any) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Gentian-Actor", "ada")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// Who holds what is the registrar's to ask for and nobody else's but the
// director's; and it is all the registrar is admitted to.
func TestAssignmentsAreTheRegistrarsAndNothingElseIs(t *testing.T) {
	_, auth := cluster()
	h := &HTTPServer{Auth: auth, Rights: demoRights()}
	mux := h.routes()
	give := "/v1/tenants/demo/people/u1/actions/give"
	for who, want := range map[string]int{
		"registrar": http.StatusOK, "director": http.StatusOK,
		"usher": http.StatusForbidden, "custodian": http.StatusForbidden, "impostor": http.StatusForbidden,
		"director-for-the-api-server": http.StatusUnauthorized, "": http.StatusUnauthorized,
	} {
		if code, _ := askAssignments(mux, "POST", give, who, `{"position":"user"}`); code != want {
			t.Errorf("%q giving a position: %d, want %d", who, code, want)
		}
	}
	// A read of who holds what is not the usher's either.
	if code, _ := askAssignments(mux, "GET", "/v1/tenants/demo/people/u1/assignments", "usher", ""); code != http.StatusForbidden {
		t.Errorf("the usher reading assignments: %d", code)
	}
	// And the registrar reaches no route of the director's or the usher's.
	guarded := guardOnly(&HTTPServer{Auth: auth})
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/tenants/demo/backups"},
		{"GET", "/v1/tenants/demo/backups/b1/download"},
		{"POST", "/v1/tenants/demo/actions/delete-backup"},
	} {
		if got := answer(guarded, c.method, c.path, "registrar"); got != http.StatusForbidden {
			t.Errorf("the registrar on %s %s: %d, want 403", c.method, c.path, got)
		}
	}
}

func TestGivingAndTakingThroughTheListener(t *testing.T) {
	_, auth := cluster()
	store := demoRights()
	mux := (&HTTPServer{Auth: auth, Rights: store}).routes()
	person := "/v1/tenants/demo/people/u1"
	admin := `{"position":"tenant-administrator"}`

	code, body := askAssignments(mux, "POST", person+"/actions/give", "registrar", admin)
	if code != http.StatusOK || body["changed"] != true {
		t.Fatalf("given: %d %v", code, body)
	}
	if code, body := askAssignments(mux, "POST", person+"/actions/give", "registrar", admin); code != http.StatusOK || body["changed"] != false {
		t.Errorf("given again: %d %v", code, body)
	}
	if code, _ := askAssignments(mux, "POST", person+"/actions/give", "registrar", `{"app":"wiki","role":"user"}`); code != http.StatusOK {
		t.Errorf("a role on an app: %d", code)
	}
	code, body = askAssignments(mux, "GET", person+"/assignments", "registrar", "")
	if list, _ := body["assignments"].([]any); code != http.StatusOK || len(list) != 2 {
		t.Errorf("assignments: %d %v", code, body)
	}
	code, body = askAssignments(mux, "GET", "/v1/tenants/demo/administrators", "registrar", "")
	if list, _ := body["subjects"].([]any); code != http.StatusOK || len(list) != 1 || list[0] != "u1" {
		t.Errorf("administrators: %d %v", code, body)
	}

	// Refused, each for what it is.
	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"a position the tenant has not":     {person + "/actions/give", `{"position":"ghost"}`, http.StatusNotFound},
		"an app the tenant has not":         {person + "/actions/give", `{"app":"notes","role":"user"}`, http.StatusNotFound},
		"what is no assignment":             {person + "/actions/give", `{"position":"user","role":"admin"}`, http.StatusBadRequest},
		"a body that is none":               {person + "/actions/give", `not json`, http.StatusBadRequest},
		"a field nobody knows":              {person + "/actions/give", `{"position":"user","tenant":"other"}`, http.StatusBadRequest},
		"a person that cannot be named":     {"/v1/tenants/demo/people/a%20b/actions/give", `{"position":"user"}`, http.StatusBadRequest},
		"another tenant, which has no such": {"/v1/tenants/other/people/u1/actions/give", `{"position":"user"}`, http.StatusNotFound},
		"the last administrator's position": {person + "/actions/take", admin, http.StatusConflict},
		"the last administrator, removed":   {person + "/actions/remove-rights", ``, http.StatusConflict},
	} {
		if code, body := askAssignments(mux, "POST", c.path, "registrar", c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", name, code, body, c.want)
		}
	}
	// With a second administrator the first can be removed, whole.
	if code, _ := askAssignments(mux, "POST", "/v1/tenants/demo/people/u2/actions/give", "registrar", admin); code != http.StatusOK {
		t.Fatalf("a second administrator: %d", code)
	}
	code, body = askAssignments(mux, "POST", person+"/actions/remove-rights", "registrar", "")
	if code != http.StatusOK || body["removed"] != float64(2) {
		t.Errorf("removed: %d %v", code, body)
	}
	for tuple := range store.tuples {
		if tuple.User == "user:u1" {
			t.Errorf("left of a removed person: %+v", tuple)
		}
	}

	// A store that does not answer is not an answer, and its error is not
	// passed on.
	store.fail = errors.New("dial tcp 10.0.0.7:8080: connection refused")
	code, body = askAssignments(mux, "POST", person+"/actions/give", "registrar", `{"position":"user"}`)
	if msg, _ := body["error"].(string); code != http.StatusBadGateway || strings.Contains(msg, "10.0.0.7") {
		t.Errorf("an unreachable store: %d %v", code, body)
	}
	// A cluster that runs no rights store says so.
	none := (&HTTPServer{Auth: auth}).routes()
	if code, _ := askAssignments(none, "POST", person+"/actions/give", "registrar", `{"position":"user"}`); code != http.StatusServiceUnavailable {
		t.Errorf("without a store: %d", code)
	}
}
