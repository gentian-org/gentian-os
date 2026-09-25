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

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// users and groups per realm.
	groups map[string][]groupRep
	users  map[string][]userRep
	// status overrides a path's answer, for the failure cases.
	status map[string]int
	// policy per realm.
	policy map[string]string
	// mints counts token requests per realm, for the cache test.
	mints map[string]int
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
		t:      t,
		groups: map[string][]groupRep{},
		users:  map[string][]userRep{},
		status: map[string]int{},
		policy: map[string]string{},
		mints:  map[string]int{},
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
		_, _ = w.Write([]byte(`{"errorMessage":"refused by the fake"}`))
		return
	}
	groups := append([]groupRep(nil), f.groups[realm]...)
	users := append([]userRep(nil), f.users[realm]...)
	policy := f.policy[realm]
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && path == "/groups":
		_ = json.NewEncoder(w).Encode(groups)
	case r.Method == http.MethodGet && path == "/users":
		_ = json.NewEncoder(w).Encode(users)
	case r.Method == http.MethodPost && path == "/users":
		w.Header().Set("Location", "https://kc/admin/realms/"+realm+"/users/abc123")
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/execute-actions-email"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/groups") && strings.HasPrefix(path, "/users/"):
		_ = json.NewEncoder(w).Encode(groups)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/users/"):
		for _, u := range users {
			if strings.HasSuffix(path, "/"+u.ID) {
				_ = json.NewEncoder(w).Encode(u)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodGet && path == "":
		_ = json.NewEncoder(w).Encode(map[string]any{"realm": realm, "passwordPolicy": policy})
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

func clientFor(t *testing.T, srv *httptest.Server, src CredentialSource) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: srv.URL, Source: src})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestARealmWithNoCredentialCannotBeNamed(t *testing.T) {
	_, srv := newFake(t)
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "d", ClientSecret: "s"}})

	if _, err := c.Realm("other"); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("realm the director holds nothing for: got %v, want ErrNoCredential", err)
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
		"demo":  {Realm: "demo", ClientID: "director-demo", ClientSecret: "s1"},
		"other": {Realm: "other", ClientID: "director-other", ClientSecret: "s2"},
	})
	demo, err := c.Realm("demo")
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.Realm("other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Groups(context.Background(), demo); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Groups(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls: %d, want 2", len(calls))
	}
	if !strings.Contains(calls[0].path, "/admin/realms/demo/") || calls[0].token != "token-for-demo-director-demo" {
		t.Errorf("demo call went to %q with %q", calls[0].path, calls[0].token)
	}
	if !strings.Contains(calls[1].path, "/admin/realms/other/") || calls[1].token != "token-for-other-director-other" {
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
		if _, err := c.Groups(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mints["demo"] != 1 || f.mints["other"] != 1 {
		t.Fatalf("token mints: demo=%d other=%d, want 1 and 1", f.mints["demo"], f.mints["other"])
	}
}

func TestInviteCreatesThePersonInTheirGroupsAndSendsOneMail(t *testing.T) {
	f, srv := newFake(t)
	f.groups["demo"] = []groupRep{{ID: "g1", Name: "members", Path: "/gentian:tenant:demo:members"}}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	p, err := c.Invite(context.Background(), r, Invitation{
		Email:       "Ada@example.com",
		Groups:      []string{"gentian:tenant:demo:members"},
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
	if !strings.Contains(create.body, `"/gentian:tenant:demo:members"`) {
		t.Errorf("the groups are part of the creation, not a second call: %s", create.body)
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

// A tenant that shares the kernel realm is confined by the group subtree,
// because the credential is no longer a boundary.
func TestAScopedRealmSeesAndTouchesOnlyItsOwnGroups(t *testing.T) {
	f, srv := newFake(t)
	f.groups["kernel"] = []groupRep{
		{ID: "g1", Name: "members", Path: "/gentian:tenant:demo:members"},
		{ID: "g2", Name: "admins", Path: "/gentian:tenant:other:admins"},
		{ID: "g3", Name: "superadmin", Path: "/gentian:platform:admin"},
	}
	c := clientFor(t, srv, StaticSource{"kernel": {Realm: "kernel", ClientID: "a", ClientSecret: "s"}})
	base, _ := c.Realm("kernel")
	r := base.Scoped("gentian:tenant:demo:")

	groups, err := c.Groups(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Path != "gentian:tenant:demo:members" {
		t.Fatalf("scope should leave one group, got %+v", groups)
	}

	err = c.SetMembership(context.Background(), r, "abc123", "gentian:platform:admin", true)
	if !errors.Is(err, ErrOutOfScope) {
		t.Fatalf("a group outside the scope: got %v, want ErrOutOfScope", err)
	}
	for _, call := range f.recorded() {
		if call.method == http.MethodPut && strings.Contains(call.path, "/groups/g3") {
			t.Fatal("the refusal must happen before the call")
		}
	}
}

func TestSetMembershipAddsAndRemoves(t *testing.T) {
	f, srv := newFake(t)
	f.groups["demo"] = []groupRep{{ID: "g1", Name: "members", Path: "/gentian:tenant:demo:members"}}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	if err := c.SetMembership(context.Background(), r, "abc123", "gentian:tenant:demo:members", true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMembership(context.Background(), r, "abc123", "gentian:tenant:demo:members", false); err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, call := range f.recorded() {
		if strings.Contains(call.path, "/users/abc123/groups/g1") {
			methods = append(methods, call.method)
		}
	}
	if len(methods) != 2 || methods[0] != http.MethodPut || methods[1] != http.MethodDelete {
		t.Fatalf("membership calls: %v, want PUT then DELETE", methods)
	}
}

func TestThePasswordPolicyIsWrittenAsAPartialRealm(t *testing.T) {
	f, srv := newFake(t)
	f.policy["demo"] = "length(8)"
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	got, err := c.PasswordPolicy(context.Background(), r)
	if err != nil || got != "length(8)" {
		t.Fatalf("read: %q, %v", got, err)
	}
	if err := c.SetPasswordPolicy(context.Background(), r, "length(12) and notUsername(undefined)"); err != nil {
		t.Fatal(err)
	}
	var write *recorded
	for i, call := range f.recorded() {
		if call.method == http.MethodPut && strings.HasSuffix(call.path, "/admin/realms/demo") {
			write = &f.calls[i]
		}
	}
	if write == nil {
		t.Fatalf("no realm write: %+v", f.recorded())
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(write.body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["passwordPolicy"] != "length(12) and notUsername(undefined)" {
		t.Fatalf("a partial representation, not the whole realm: %v", body)
	}
}

func TestAPersonsOutOfScopeGroupsAreNotReported(t *testing.T) {
	f, srv := newFake(t)
	f.users["kernel"] = []userRep{{ID: "abc123", Username: "ada@example.com", Email: "ada@example.com", Enabled: true, EmailVerified: true}}
	f.groups["kernel"] = []groupRep{
		{ID: "g1", Path: "/gentian:tenant:demo:members"},
		{ID: "g2", Path: "/gentian:tenant:other:admins"},
	}
	c := clientFor(t, srv, StaticSource{"kernel": {Realm: "kernel", ClientID: "a", ClientSecret: "s"}})
	base, _ := c.Realm("kernel")
	r := base.Scoped("gentian:tenant:demo:")

	p, err := c.Person(context.Background(), r, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 1 || p.Groups[0] != "gentian:tenant:demo:members" {
		t.Fatalf("another tenant's membership leaked: %+v", p.Groups)
	}
	if p.Pending {
		t.Error("a verified person with no required actions is not pending")
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
// joins Keycloak's record of WHAT changed to the director's record of WHO was
// allowed to ask. Keycloak's own event names this service account and nobody
// else.
func TestTheRequestIdTravelsWithEveryAdminCall(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	f.groups["demo"] = []groupRep{{ID: "g1", Path: "/gentian:tenant:demo:members"}}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")

	ctx := WithRequestID(context.Background(), "req-42")
	if err := c.SetMembership(ctx, r, "abc123", "gentian:tenant:demo:members", true); err != nil {
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

	if _, err := c.Groups(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.recordedHeaders() {
		if v != "" {
			t.Fatalf("an empty request id must not become a header: %q", v)
		}
	}
}
