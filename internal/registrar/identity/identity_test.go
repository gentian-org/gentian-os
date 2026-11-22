/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeKeycloak is enough of the admin API to exercise this package: it mints
// a token per client and records every admin call with the token it carried,
// which is what the realm-confinement tests are really asserting.
type fakeKeycloak struct {
	t *testing.T

	mu    sync.Mutex
	calls []recorded
	// users per realm.
	users map[string][]userRep
	// status overrides a path's answer, for the failure cases.
	status map[string]int
	// message is what a path overridden by status says went wrong.
	message map[string]string
	// clients per realm, for the zone-landing read.
	clients map[string][]clientRep
	// mints counts token requests per realm, for the cache test.
	mints map[string]int
	// creds per user id, for the two-factor reads.
	creds map[string][]map[string]string
	// sessions per user id, for the session reads.
	sessions map[string][]map[string]any
}

type recorded struct {
	method    string
	path      string
	query     string
	token     string
	body      string
	requestID string
}

func newFake(t *testing.T) (*fakeKeycloak, *httptest.Server) {
	f := &fakeKeycloak{
		t:       t,
		users:   map[string][]userRep{},
		status:  map[string]int{},
		clients: map[string][]clientRep{},
		mints:   map[string]int{},
		creds:   map[string][]map[string]string{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeKeycloak) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	// Token endpoint: /realms/<r>/protocol/openid-connect/token
	if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
		realm := strings.TrimPrefix(r.URL.Path, "/realms/")
		realm = strings.TrimSuffix(realm, "/protocol/openid-connect/token")
		// The body was already drained above, so the form is parsed from it
		// rather than from the request.
		form, _ := url.ParseQuery(string(body))
		f.mu.Lock()
		f.mints[realm]++
		f.mu.Unlock()
		// The token names the realm it was minted in, so a recorded admin
		// call proves which credential made it.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-for-" + realm + "-" + form.Get("client_id"),
			"expires_in":   300,
		})
		return
	}

	// The device password endpoint the platform's Keycloak extension serves.
	if strings.HasPrefix(r.URL.Path, "/realms/") && strings.Contains(r.URL.Path, "/gentian-device-passwords/") {
		f.mu.Lock()
		f.calls = append(f.calls, recorded{method: r.Method, path: r.URL.Path, body: string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Name string   `json:"name"`
			Ways []string `json:"ways"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Name == "taken" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"there is a device password named taken already"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "dp1", "name": req.Name, "ways": req.Ways, "username": "ada",
			"password": "k3m9x-p2hrt-aaaaa-bbbbb-ccccc", "createdAt": 1794930085299,
		})
		return
	}

	// The activation-link endpoint the platform's Keycloak extension serves.
	if strings.HasPrefix(r.URL.Path, "/realms/") && strings.Contains(r.URL.Path, "/gentian-activation/") {
		f.mu.Lock()
		f.calls = append(f.calls, recorded{method: r.Method, path: r.URL.Path, body: string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"link":"https://id.example/auth/realms/demo/login-actions/action-token?key=abc","expiresAt":1700000000}`))
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/admin/realms/")
	realm, path, _ := strings.Cut(rest, "/")
	if path != "" {
		path = "/" + path
	}
	f.mu.Lock()
	f.calls = append(f.calls, recorded{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		token:     strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		body:      string(body),
		requestID: r.Header.Get(RequestIDHeader),
	})
	if code, ok := f.status[r.Method+" "+path]; ok {
		f.mu.Unlock()
		w.WriteHeader(code)
		message := "refused by the fake"
		if m, ok := f.message[r.Method+" "+path]; ok {
			message = m
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"errorMessage": message})
		return
	}
	users := append([]userRep(nil), f.users[realm]...)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && path == "/clients":
		wanted := r.URL.Query().Get("clientId")
		out := []clientRep{}
		for _, c := range f.clients[realm] {
			if c.ClientID == wanted {
				out = append(out, c)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && path == "/users":
		_ = json.NewEncoder(w).Encode(users)
	case r.Method == http.MethodPost && path == "/users":
		w.Header().Set("Location", "https://kc/admin/realms/"+realm+"/users/abc123")
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/execute-actions-email"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/credentials") && strings.HasPrefix(path, "/users/"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/credentials")
		f.mu.Lock()
		out := f.creds[id]
		f.mu.Unlock()
		if out == nil {
			out = []map[string]string{}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/sessions") && strings.HasPrefix(path, "/users/"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/sessions")
		f.mu.Lock()
		out := f.sessions[id]
		f.mu.Unlock()
		if out == nil {
			out = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/users/"):
		for _, u := range users {
			if strings.HasSuffix(path, "/"+u.ID) {
				_ = json.NewEncoder(w).Encode(u)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeKeycloak) recordedHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.requestID)
	}
	return out
}

func (f *fakeKeycloak) recorded() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.calls...)
}

// roleHolders names the platform tenant's role holders the way the
// registrar's reading of the Cluster claim does.
func roleHolders(subjects ...string) RoleHolders {
	return func(context.Context) ([]string, error) { return subjects, nil }
}

// clientFor is a client on a cluster whose claim names no role holder.
func clientFor(t *testing.T, srv *httptest.Server, src CredentialSource) *Client {
	t.Helper()
	return clientProtecting(t, srv, src)
}

// clientProtecting is a client told that these accounts hold cluster roles.
func clientProtecting(t *testing.T, srv *httptest.Server, src CredentialSource, subjects ...string) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: srv.URL, Source: src, PlatformRoleHolders: roleHolders(subjects...)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// only is a scope of exactly these accounts.
func only(subjects ...string) Subjects {
	return func(context.Context) (map[string]bool, error) {
		in := map[string]bool{}
		for _, s := range subjects {
			in[s] = true
		}
		return in, nil
	}
}

func TestARealmWithNoCredentialCannotBeNamed(t *testing.T) {
	_, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "d", ClientSecret: "s"}})

	if _, err := c.Realm("other"); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("realm the registrar holds nothing for: got %v, want ErrNoCredential", err)
	}
	if _, err := c.Realm("demo"); err != nil {
		t.Fatalf("realm it does hold: %v", err)
	}
}

// The property the package exists for: a handler holding realm A's token
// cannot produce a call against realm B, because the realm is in the type and
// the URL is built from it.
func TestEveryCallIsScopedToItsOwnRealm(t *testing.T) {
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{
		"demo":  {Realm: "demo", ClientID: "registrar-demo", ClientSecret: "s1"},
		"other": {Realm: "other", ClientID: "registrar-other", ClientSecret: "s2"},
	})
	demo, err := c.Realm("demo")
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.Realm("other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.People(context.Background(), demo, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.People(context.Background(), other, "", 0); err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls: %d, want 2", len(calls))
	}
	if !strings.Contains(calls[0].path, "/admin/realms/demo/") || calls[0].token != "token-for-demo-registrar-demo" {
		t.Errorf("demo call went to %q with %q", calls[0].path, calls[0].token)
	}
	if !strings.Contains(calls[1].path, "/admin/realms/other/") || calls[1].token != "token-for-other-registrar-other" {
		t.Errorf("other call went to %q with %q", calls[1].path, calls[1].token)
	}
}

func TestATokenIsCachedPerRealm(t *testing.T) {
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{
		"demo":  {Realm: "demo", ClientID: "a", ClientSecret: "s"},
		"other": {Realm: "other", ClientID: "b", ClientSecret: "s"},
	})
	for _, name := range []string{"demo", "demo", "other", "demo"} {
		r, err := c.Realm(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.People(context.Background(), r, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mints["demo"] != 1 || f.mints["other"] != 1 {
		t.Fatalf("token mints: demo=%d other=%d, want 1 and 1", f.mints["demo"], f.mints["other"])
	}
}

func TestInviteCreatesThePersonInNoGroupAndSendsOneMail(t *testing.T) {
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	p, err := c.Invite(context.Background(), r, Invitation{
		Email:       "Ada@example.com",
		ClientID:    "gentian-portal",
		RedirectURI: "https://demo.example.org/",
	})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if p.Email != "ada@example.com" || p.Username != "ada@example.com" {
		t.Errorf("address not normalised: %+v", p)
	}
	if !p.Pending {
		t.Error("a person who has not set a password is pending")
	}

	var create, mail *recorded
	for i, c := range f.recorded() {
		switch {
		case c.method == http.MethodPost && strings.HasSuffix(c.path, "/users"):
			create = &f.calls[i]
		case c.method == http.MethodPut && strings.HasSuffix(c.path, "/execute-actions-email"):
			mail = &f.calls[i]
		}
	}
	if create == nil || mail == nil {
		t.Fatalf("expected a creation and one mail, got %+v", f.recorded())
	}
	if strings.Contains(create.body, `"groups"`) {
		t.Errorf("what a person holds is the rights store's, not a group of the realm: %s", create.body)
	}
	if !strings.Contains(mail.body, "UPDATE_PASSWORD") || !strings.Contains(mail.body, "VERIFY_EMAIL") {
		t.Errorf("one link for both actions: %s", mail.body)
	}
	if !strings.Contains(mail.query, "client_id=gentian-portal") {
		t.Errorf("the action token needs the client: %s", mail.query)
	}
}

func TestInvitingSomebodyWhoIsAlreadyThereIsAConflict(t *testing.T) {
	f, srv := newFake(t)
	f.status["POST /users"] = http.StatusConflict
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	_, err := c.Invite(context.Background(), r, Invitation{Email: "ada@example.com"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
}

// The half-done case: the person exists and the mail did not go. The answer
// has to say both, because the repair is to re-send rather than to invite.
func TestAnInvitationWhoseMailFailsStillReportsThePerson(t *testing.T) {
	f, srv := newFake(t)
	f.status["PUT /users/abc123/execute-actions-email"] = http.StatusBadGateway
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	p, err := c.Invite(context.Background(), r, Invitation{Email: "ada@example.com"})
	if err == nil {
		t.Fatal("a mail that did not go is an error")
	}
	if p.ID == "" {
		t.Errorf("the person was created and the answer must say so: %+v", p)
	}
	if !strings.Contains(err.Error(), "the mail was not sent") {
		t.Errorf("error should name what failed: %v", err)
	}
}

func TestAnAddressIsCheckedBeforeAnythingIsCreated(t *testing.T) {
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	if _, err := c.Invite(context.Background(), r, Invitation{Email: "not-an-address"}); err == nil {
		t.Fatal("expected a refusal")
	}
	if len(f.recorded()) != 0 {
		t.Errorf("nothing should reach Keycloak: %+v", f.recorded())
	}
}

func TestAnIdWithAPathSeparatorIsRefused(t *testing.T) {
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	if _, err := c.Person(context.Background(), r, "../../realms/other/users"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if len(f.recorded()) != 0 {
		t.Errorf("nothing should reach Keycloak: %+v", f.recorded())
	}
}

// The request id has to reach Keycloak, because it is the only thing that
// joins Keycloak's record of WHAT changed to the registrar's record of WHO was
// allowed to ask. Keycloak's own event names this service account and nobody
// else.
func TestTheRequestIdTravelsWithEveryAdminCall(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	ctx := WithRequestID(context.Background(), "req-42")
	if err := c.RemovePerson(ctx, r, "abc123"); err != nil {
		t.Fatal(err)
	}
	var admin int
	for _, call := range f.recordedHeaders() {
		if call == "req-42" {
			admin++
		}
	}
	if admin == 0 {
		t.Fatal("no admin call carried the request id")
	}
}

func TestACallWithNoRequestIdSendsNoHeader(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	if _, err := c.People(context.Background(), r, "", 0); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.recordedHeaders() {
		if v != "" {
			t.Fatalf("an empty request id must not become a header: %q", v)
		}
	}
}

// ZoneLanding reads the landing page off the client, and normalizes the
// trailing slash: the permitted URI carries one, rootUrl conventionally does
// not, and a redirect that differs from the permitted URI by that slash is
// refused.
func TestZoneLandingReadsTheClientsRoot(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	f.clients["demo"] = []clientRep{{ClientID: "gentian-edge-demo", RootURL: "https://desktop.demo.example.test"}}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	if got := c.ZoneLanding(context.Background(), r, "gentian-edge-demo"); got != "https://desktop.demo.example.test/" {
		t.Fatalf("landing = %q", got)
	}
	// A client that states no root yields nothing, and an invitation then
	// carries no redirect rather than one Keycloak refuses.
	if got := c.ZoneLanding(context.Background(), r, "gentian-edge-other"); got != "" {
		t.Fatalf("landing for an unknown client = %q, want empty", got)
	}
}

// clientRep is the part of a Keycloak client representation the landing read
// needs.
type clientRep struct {
	ClientID string `json:"clientId"`
	RootURL  string `json:"rootUrl"`
	BaseURL  string `json:"baseUrl"`
}

func (f *fakeKeycloak) callsTo(method, suffix string) []recorded {
	var out []recorded
	for _, c := range f.recorded() {
		if c.method == method && strings.HasSuffix(c.path, suffix) {
			out = append(out, c)
		}
	}
	return out
}

// The login and the mailbox are two addresses: <local>@<tenant domain> to
// sign in with, and the address the invitation reaches. Keycloak mails the
// email field, so that is the delivery address, mirrored in an attribute.
func TestInviteSeparatesTheLoginFromTheMailbox(t *testing.T) {
	f, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	p, err := c.Invite(context.Background(), r, Invitation{
		Email: "Jane@Example.org", Username: "jane-doe@demo.k.example",
		FirstName: " Jane ", LastName: "Doe", RequireTOTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Username != "jane-doe@demo.k.example" || p.Email != "jane@example.org" || !p.TOTPRequired {
		t.Fatalf("person: %+v", p)
	}
	posts := f.callsTo(http.MethodPost, "/users")
	if len(posts) != 1 {
		t.Fatalf("one creation, got %d", len(posts))
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(posts[0].body), &body)
	if body["username"] != "jane-doe@demo.k.example" || body["email"] != "jane@example.org" ||
		body["firstName"] != "Jane" || body["lastName"] != "Doe" {
		t.Fatalf("created with: %v", body)
	}
	attrs, _ := body["attributes"].(map[string]any)
	if v, _ := attrs["gentian.inviteEmail"].([]any); len(v) != 1 || v[0] != "jane@example.org" {
		t.Fatalf("delivery address not mirrored: %v", attrs)
	}
	mails := f.callsTo(http.MethodPut, "/execute-actions-email")
	if len(mails) != 1 || !strings.Contains(mails[0].body, "CONFIGURE_TOTP") {
		t.Fatalf("two-factor must be part of accepting: %+v", mails)
	}
}

func TestRemoveTOTPDeletesOnlyAuthenticatorsAndTheRequirement(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "ada@example.com", Enabled: true,
		RequiredActions: []string{"CONFIGURE_TOTP", "UPDATE_PROFILE"}}}
	f.creds["abc123"] = []map[string]string{{"id": "pw1", "type": "password"}, {"id": "otp1", "type": "otp"}}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	if err := c.RemoveTOTP(context.Background(), r, "abc123"); err != nil {
		t.Fatal(err)
	}
	dels := f.callsTo(http.MethodDelete, "/credentials/otp1")
	if len(dels) != 1 || len(f.callsTo(http.MethodDelete, "/credentials/pw1")) != 0 {
		t.Fatalf("only the authenticator goes: %+v", f.recorded())
	}
	puts := f.callsTo(http.MethodPut, "/users/abc123")
	if len(puts) != 1 || strings.Contains(puts[0].body, "CONFIGURE_TOTP") || !strings.Contains(puts[0].body, "UPDATE_PROFILE") {
		t.Fatalf("the requirement goes and the others stay: %+v", puts)
	}
}

// With nowhere to mail it, the link comes back to show once -- and asks for a
// second factor when one is required and not yet enrolled.
func TestActivationWithoutAnAddressReturnsTheLink(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "admin@demo.k.example", Enabled: true}}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	a, err := c.ActivateAccount(context.Background(), r, "abc123", "", true, "gentian-edge-demo", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Mailed || a.Link == "" || a.ExpiresAt == 0 {
		t.Fatalf("activation: %+v", a)
	}
	links := f.callsTo(http.MethodPost, "/gentian-activation/users/abc123/link")
	if len(links) != 1 || !strings.Contains(links[0].body, "CONFIGURE_TOTP") || !strings.Contains(links[0].body, "UPDATE_PASSWORD") {
		t.Fatalf("link asked with: %+v", links)
	}
	if len(f.callsTo(http.MethodPut, "/execute-actions-email")) != 0 {
		t.Fatal("nothing should be mailed without an address")
	}
	// A link that is shown has reached no address, so it must not be one
	// Keycloak takes as proof of an address.
	if strings.Contains(links[0].body, "VERIFY_EMAIL") {
		t.Fatalf("a shown link must not verify an address: %+v", links)
	}
}

// An account whose address is its login, in a realm that delivers nothing
// there, is handed over by a link that is shown: mailed, it would reach
// nobody. The address it has is not the one mailed to, and no other is given
// to it.
func TestAShownOnlyActivationMailsNothing(t *testing.T) {
	f, srv := newFake(t)
	f.users["customers"] = []userRep{{ID: "abc123", Username: "admin@customers.k.example", Email: "admin@customers.k.example", Enabled: true}}
	c := clientFor(t, srv, StaticSource{"customers": {Realm: "customers", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("customers")
	ctx := WithShownOnly(context.Background())

	a, err := c.ActivateAccount(ctx, r, "abc123", "", true, "gentian-edge-customers", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Mailed || a.Link == "" {
		t.Fatalf("activation: %+v", a)
	}
	if len(f.callsTo(http.MethodPut, "/execute-actions-email")) != 0 {
		t.Fatal("the link was mailed to the account's own address")
	}

	if _, err := c.ActivateAccount(ctx, r, "abc123", "owner@example.org", true, "", ""); err == nil {
		t.Fatal("an address was accepted for an account whose link is only shown")
	}
	for _, put := range f.callsTo(http.MethodPut, "/users/abc123") {
		if strings.Contains(put.body, "owner@example.org") {
			t.Fatalf("the account was given the address: %+v", put)
		}
	}
}

// Given an address, it becomes the recovery address and the link is mailed
// there; an enrolled second factor is not asked for again.
func TestActivationWithAnAddressMailsIt(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "admin@demo.k.example", Enabled: true}}
	f.creds["abc123"] = []map[string]string{{"id": "o1", "type": "otp"}}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	a, err := c.ActivateAccount(context.Background(), r, "abc123", "Owner@Example.org", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Mailed || a.Email != "owner@example.org" || a.Link != "" {
		t.Fatalf("activation: %+v", a)
	}
	puts := f.callsTo(http.MethodPut, "/users/abc123")
	if len(puts) != 1 || !strings.Contains(puts[0].body, `"email":"owner@example.org"`) {
		t.Fatalf("the address must become the recovery address: %+v", puts)
	}
	mails := f.callsTo(http.MethodPut, "/execute-actions-email")
	if len(mails) != 1 || strings.Contains(mails[0].body, "CONFIGURE_TOTP") {
		t.Fatalf("an enrolled factor is not asked for again: %+v", mails)
	}
	// Keycloak records the address as verified only when the mailed link
	// names VERIFY_EMAIL; without it the administrator stays "pending".
	if !strings.Contains(mails[0].body, "VERIFY_EMAIL") || !strings.Contains(mails[0].body, "UPDATE_PASSWORD") {
		t.Fatalf("the mailed link must verify the address it was sent to: %+v", mails)
	}
	if strings.Contains(puts[0].body, "VERIFY_EMAIL") {
		t.Fatalf("VERIFY_EMAIL belongs to the mailed link, not to the account: %+v", puts)
	}
}

// A realm that cannot mail yet -- an external relay whose credential arrives
// through the console, after this very sign-in -- still hands the account
// over: the link is shown instead, the address stays the recovery address,
// and the answer says why it was not mailed.
func TestActivationFallsBackToTheLinkWhenTheMailFails(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "admin@demo.k.example", Enabled: true}}
	f.status["PUT /users/abc123/execute-actions-email"] = http.StatusInternalServerError
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	a, err := c.ActivateAccount(context.Background(), r, "abc123", "owner@example.org", true, "gentian-edge-demo", "")
	if err != nil {
		t.Fatalf("a mail that cannot go must not refuse the activation: %v", err)
	}
	if a.Mailed || a.Link == "" || a.Email != "owner@example.org" || a.MailError == "" {
		t.Fatalf("activation: %+v", a)
	}
	// The reason is the realm's own: the status and what it said.
	if !strings.Contains(a.MailError, "HTTP 500") || !strings.Contains(a.MailError, "refused by the fake") {
		t.Fatalf("the reason Keycloak gave must be passed on: %q", a.MailError)
	}
	if strings.Contains(a.MailError, "abc123") || strings.Contains(a.MailError, "/users/") {
		t.Fatalf("the reason names the call, which says nothing to its reader: %q", a.MailError)
	}
	if puts := f.callsTo(http.MethodPut, "/users/abc123"); len(puts) != 1 || !strings.Contains(puts[0].body, `"email":"owner@example.org"`) {
		t.Fatalf("the address must still become the recovery address: %+v", puts)
	}
}

// What Keycloak says went wrong can name the address it was sending to, and a
// link. Neither is passed on or logged; the rest of the reason is, on one
// line and bounded.
func TestTheReasonAMailFailedCarriesNoAddressAndNoLink(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "admin@demo.k.example", Enabled: true}}
	f.status["PUT /users/abc123/execute-actions-email"] = http.StatusInternalServerError
	f.message = map[string]string{"PUT /users/abc123/execute-actions-email": "Failed to send execute actions email:\n 535 Authentication failed for <owner@example.org> via smtp://relay.example.org:587/x?key=secret " +
		strings.Repeat("and more ", 100)}
	var logged bytes.Buffer
	c, err := New(Config{
		BaseURL:             srv.URL,
		Source:              StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}},
		PlatformRoleHolders: roleHolders(),
		Logger:              slog.New(slog.NewTextHandler(&logged, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := c.Realm("demo")

	a, err := c.ActivateAccount(context.Background(), r, "abc123", "owner@example.org", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{"the answer": a.MailError, "the log": logged.String()} {
		if !strings.Contains(text, "HTTP 500: Failed to send execute actions email: 535 Authentication failed") {
			t.Errorf("%s should carry the reason: %q", what, text)
		}
		for _, secret := range []string{"owner@example.org", "relay.example.org", "key=secret", a.Link} {
			if strings.Contains(text, secret) {
				t.Errorf("%s carries %q: %q", what, secret, text)
			}
		}
	}
	if strings.ContainsAny(a.MailError, "\r\n") || len([]rune(a.MailError)) > 300 {
		t.Errorf("the reason is one bounded line: %d runes %q", len([]rune(a.MailError)), a.MailError)
	}
	if !strings.Contains(logged.String(), "level=WARN") {
		t.Errorf("a mail that did not go is a warning: %q", logged.String())
	}
}

// An activation nobody is waiting for is mailed or not issued: where the realm
// cannot mail, no link is made at all, and the address still becomes the
// recovery address.
func TestAMailOnlyActivationMakesNoLinkWhenTheMailFails(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "admin@demo.k.example", Enabled: true}}
	f.status["PUT /users/abc123/execute-actions-email"] = http.StatusInternalServerError
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	a, err := c.ActivateAccount(WithMailOnly(context.Background()), r, "abc123", "owner@example.org", true, "gentian-edge-demo", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Mailed || a.Link != "" || a.MailError == "" {
		t.Fatalf("activation: %+v", a)
	}
	if links := f.callsTo(http.MethodPost, "/link"); len(links) != 0 {
		t.Fatalf("no link may be issued with nobody to show it to: %+v", links)
	}
	if puts := f.callsTo(http.MethodPut, "/users/abc123"); len(puts) != 1 || !strings.Contains(puts[0].body, `"email":"owner@example.org"`) {
		t.Fatalf("the address must still become the recovery address: %+v", puts)
	}
}

func TestDevicePasswordsAreMadeListedAndRevoked(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "ada", Enabled: true}}
	f.creds["abc123"] = []map[string]string{
		{"id": "pw1", "type": "password"},
		{"id": "otp1", "type": "otp"},
		{"id": "dp1", "type": DevicePasswordType, "userLabel": "Ada's phone",
			"credentialData": `{"ref":"k3m9x","ways":["calendars","contacts"],"lastUsedAt":1794930085383}`},
	}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")
	ctx := context.Background()

	made, err := c.CreateDevicePassword(ctx, r, "abc123", "Ada's phone", []string{"calendars", "contacts"})
	if err != nil || made.Password == "" || made.Username != "ada" || made.ID != "dp1" {
		t.Fatalf("made %+v, %v", made, err)
	}
	if posts := f.callsTo(http.MethodPost, "/realms/demo/gentian-device-passwords/users/abc123"); len(posts) != 1 {
		t.Fatalf("the extension was asked %+v", f.recorded())
	}
	if _, err := c.CreateDevicePassword(ctx, r, "abc123", "taken", []string{"files"}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "named taken already") {
		t.Fatalf("a name already used: %v", err)
	}

	held, err := c.DevicePasswords(ctx, r, "abc123")
	if err != nil || len(held) != 1 || held[0].Name != "Ada's phone" || strings.Join(held[0].Ways, ",") != "calendars,contacts" ||
		held[0].LastUsedAt == nil {
		t.Fatalf("listed %+v, %v", held, err)
	}

	// Only a device password is removed; a second factor named by its id is
	// not one, and is not touched.
	if err := c.RevokeDevicePassword(ctx, r, "abc123", "otp1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a second factor: %v", err)
	}
	if err := c.RevokeDevicePassword(ctx, r, "abc123", "dp1"); err != nil {
		t.Fatal(err)
	}
	if dels := f.callsTo(http.MethodDelete, "/credentials/dp1"); len(dels) != 1 || len(f.callsTo(http.MethodDelete, "/credentials/otp1")) != 0 {
		t.Fatalf("removed %+v", f.recorded())
	}
}
