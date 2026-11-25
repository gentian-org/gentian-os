/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"context"
	"net/http"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// The fake's half of sessions: kept per realm and person.

func (f *fakeIdentity) Sessions(ctx context.Context, r identity.Realm, id string) ([]identity.Session, error) {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]identity.Session{}, f.sessions[r.Name()+"/"+id]...), nil
}

func (f *fakeIdentity) EndSession(ctx context.Context, r identity.Realm, id, sessionID string) error {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Name() + "/" + id
	for i, s := range f.sessions[key] {
		if s.ID == sessionID {
			f.sessions[key] = append(f.sessions[key][:i], f.sessions[key][i+1:]...)
			return nil
		}
	}
	return identity.ErrNotFound
}

const (
	ownAccount          = "/v1/tenants/demo/account"
	ownSessions         = "/v1/tenants/demo/account/sessions"
	updateOwnProfile    = "/v1/tenants/demo/actions/update-own-profile"
	endOwnSession       = "/v1/tenants/demo/actions/end-own-session"
	endOwnOtherSessions = "/v1/tenants/demo/actions/end-own-other-sessions"
)

// accountFake is a realm where mia and tom each have sessions.
func accountFake() *fakeIdentity {
	f := vouchingFake()
	f.people = map[string]identity.Person{
		"mia": {ID: "mia", Username: "mia", Email: "mia@example.com", FirstName: "Mia", TOTPConfigured: true},
		"tom": {ID: "tom", Username: "tom", Email: "tom@example.com"},
	}
	f.sessions = map[string][]identity.Session{
		"demo/mia": {{ID: "s-here"}, {ID: "s-phone"}, {ID: "s-laptop"}},
		"demo/tom": {{ID: "s-tom"}},
	}
	return f
}

// inSession mints a token of demo for sub, belonging to the realm's session sid.
func (h *harness) inSession(t *testing.T, sub, sid string) string {
	return h.issuer.Token(t, dt.Claims{Realm: "demo", Subject: sub, Audience: audience, Name: sub, Email: sub + "@example.com",
		Extra: map[string]any{"sid": sid}})
}

// A person reads their own account and changes their names, and only those:
// a body that names an address or somebody else is not read at all.
func TestAPersonReadsAndRenamesTheirOwnAccount(t *testing.T) {
	f := accountFake()
	h := startVouching(t, f, nil)
	mia := h.inSession(t, "mia", "s-here")

	status, body := h.do(t, http.MethodGet, ownAccount, mia, "")
	if status != http.StatusOK || body["email"] != "mia@example.com" || body["firstName"] != "Mia" || body["totpConfigured"] != true {
		t.Fatalf("reading: %d %v", status, body)
	}
	for _, refused := range []string{
		`{"email":"evil@example.com"}`,
		`{"firstName":"Mia","person":"tom"}`,
		`{}`,
	} {
		if status, _ := h.do(t, http.MethodPost, updateOwnProfile, mia, refused); status != http.StatusBadRequest {
			t.Fatalf("%s: %d", refused, status)
		}
	}
	if len(f.updates) != 0 {
		t.Fatalf("a refused body changed something: %v", f.updates)
	}
	if status, _ := h.do(t, http.MethodPost, updateOwnProfile, mia, `{"firstName":"Mia","lastName":"Muster"}`); status != http.StatusOK {
		t.Fatalf("renaming: %d", status)
	}
	u, ok := f.updates["demo/mia"]
	if !ok || len(f.updates) != 1 || u.Email != nil || u.Enabled != nil || u.LastName == nil || *u.LastName != "Muster" {
		t.Fatalf("updated %v", f.updates)
	}
}

// A person sees where they are signed in, with the session of this page
// marked, ends one, and ends all the others; nobody else's is touched.
func TestAPersonEndsTheirOwnSessionsAndKeepsThisOne(t *testing.T) {
	f := accountFake()
	h := startVouching(t, f, nil)
	mia := h.inSession(t, "mia", "s-here")

	status, body := h.do(t, http.MethodGet, ownSessions, mia, "")
	list, _ := body["sessions"].([]any)
	if status != http.StatusOK || len(list) != 3 {
		t.Fatalf("listing: %d %v", status, body)
	}
	if first, _ := list[0].(map[string]any); first["id"] != "s-here" || first["current"] != true {
		t.Fatalf("this page's session is not marked: %v", list[0])
	}
	if status, _ := h.do(t, http.MethodPost, endOwnSession, mia, `{"id":"s-tom"}`); status != http.StatusNotFound {
		t.Fatalf("ending tom's session: %d", status)
	}
	if status, _ := h.do(t, http.MethodPost, endOwnSession, mia, `{"id":"s-phone"}`); status != http.StatusNoContent {
		t.Fatalf("ending one: %d", status)
	}
	if status, _ := h.do(t, http.MethodPost, endOwnOtherSessions, mia, ``); status != http.StatusNoContent {
		t.Fatalf("ending the others: %d", status)
	}
	if got := f.sessions["demo/mia"]; len(got) != 1 || got[0].ID != "s-here" {
		t.Fatalf("mia's sessions left: %v", got)
	}
	if got := f.sessions["demo/tom"]; len(got) != 1 {
		t.Fatalf("tom's sessions: %v", got)
	}
}

// A token that names no session ends no session wholesale: which one to keep
// would be a guess, and a wrong one signs the page out.
func TestEndingTheOtherSessionsNeedsToKnowThisOne(t *testing.T) {
	f := accountFake()
	h := startVouching(t, f, nil)
	noSession := h.issuer.Token(t, dt.Claims{Realm: "demo", Subject: "mia", Audience: audience, Extra: map[string]any{"sid": ""}})
	if status, _ := h.do(t, http.MethodPost, endOwnOtherSessions, noSession, ``); status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
	if got := f.sessions["demo/mia"]; len(got) != 3 {
		t.Fatalf("sessions ended: %v", got)
	}
}

// The account is kept in the tenant's realm, so it is read and changed with
// a token of that realm only.
func TestAnAccountIsReachedWithATokenOfTheTenantsRealm(t *testing.T) {
	f := accountFake()
	h := startVouching(t, f, nil)
	tina := h.token(t, "solo", "tina")
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, ownAccount, ""},
		{http.MethodGet, ownSessions, ""},
		{http.MethodPost, updateOwnProfile, `{"firstName":"x"}`},
		{http.MethodPost, endOwnOtherSessions, ""},
	} {
		if status, _ := h.do(t, call.method, call.path, tina, call.body); status != http.StatusForbidden {
			t.Fatalf("%s %s with another realm's token: %d", call.method, call.path, status)
		}
	}
}
