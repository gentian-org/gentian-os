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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

// The contract tests run the whole registrar -- token verification, the
// OpenFGA question, the call to the realm -- against a static-key issuer.
//
// The people are those of authz/model/v1/tests.fga.yaml, as in the director's
// tests: tom administers tenant demo, mia is a member of it, tina administers
// tenant solo, alice is the platform administrator.
//
// With DIRECTOR_TEST_OPENFGA_URL set, decisions come from a real OpenFGA
// holding model v1 and that fixture (`make test-director-contract`). Without
// it they come from a table that states the same facts.

const audience = "gentian-director"

// platformAdmins is the group the registrar must not touch.
const platformAdmins = "gentian:platform:admin"

// facts is what model v1 answers for the fixture, for the questions these
// tests ask.
var facts = dt.Table{
	"user:tom can_manage_users tenant:demo":         true,
	"user:tom can_set_policy tenant:demo":           true,
	"user:alice can_manage_users tenant:demo":       true,
	"user:alice can_set_policy tenant:demo":         true,
	"user:alice can_audit cluster:demo-cluster":     true,
	"user:alice can_configure cluster:demo-cluster": true,
	// Who may approve a tenant's public addresses, which every write asks
	// besides: whoever may not is held back from changing who does
	// (perimeter_test.go). The fixture's demo is a tenant whose own
	// administrators approve, and one its cluster operates; solo is neither,
	// and tina administers it.
	"user:tom can_expose tenant:demo":        true,
	"user:alice can_expose tenant:demo":      true,
	"user:tina can_manage_users tenant:solo": true,
}

// fakeTenants is what the cluster says about its tenants.
type fakeTenants map[string]registrar.Tenant

func (f fakeTenants) Tenant(_ context.Context, name string) (registrar.Tenant, error) {
	t, ok := f[name]
	if !ok {
		return registrar.Tenant{}, registrar.ErrTenantNotFound
	}
	return t, nil
}

func (f fakeTenants) All(context.Context) ([]registrar.Tenant, error) {
	out := make([]registrar.Tenant, 0, len(f))
	for _, t := range f {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ownRealm is a tenant with a realm to itself, as the operator reports it.
func ownRealm(name string) registrar.Tenant {
	return registrar.Tenant{
		Name: name, Realm: name, LoginDomain: name + "." + dt.KernelDomain, AdminRequiresMFA: true,
	}
}

// asked records the questions a Checker is put, so a test can say what a
// route asks and, as much to the point, what it does not.
type asked struct {
	authz.Checker
	mu   sync.Mutex
	seen []string
}

func (a *asked) Check(ctx context.Context, id, user, relation, object string) (bool, error) {
	a.mu.Lock()
	a.seen = append(a.seen, user+" "+relation+" "+object)
	a.mu.Unlock()
	return a.Checker.Check(ctx, id, user, relation, object)
}

func (a *asked) questions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

type harness struct {
	*httptest.Server
	issuer *dt.Issuer
	asked  *asked
}

// startWithIdentity is the registrar speaking for realms through ident.
func startWithIdentity(t *testing.T, ident registrar.Identity) *harness {
	t.Helper()
	return startWith(t, ident, nil)
}

func startWith(t *testing.T, ident registrar.Identity, decides authz.Checker) *harness {
	t.Helper()
	return startWithTenants(t, ident, decides,
		fakeTenants{"demo": ownRealm("demo"), "solo": ownRealm("solo"), "other": ownRealm("other")}, newFakeMailboxes())
}

// startWithTenants is the registrar over the tenants and the record of
// removed mailboxes given.
func startWithTenants(t *testing.T, ident registrar.Identity, decides authz.Checker, tenants fakeTenants, mailboxes registrar.Mailboxes) *harness {
	t.Helper()
	is := dt.NewIssuer(t, "gentian", "tenant-demo", "tenant-solo")
	v, err := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	if decides == nil {
		decides = dt.Checker(t, facts)
	}
	decisions := &asked{Checker: decides}
	srv, err := registrar.New(registrar.Config{
		Authn: v, Authz: decisions, Cluster: dt.Cluster,
		Tenants:   tenants,
		Identity:  ident,
		Mailboxes: mailboxes,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{Server: httptest.NewServer(srv), issuer: is, asked: decisions}
	t.Cleanup(h.Close)
	return h
}

func (h *harness) token(t *testing.T, realm, sub string) string {
	return h.issuer.Token(t, dt.Claims{Realm: realm, Subject: sub, Audience: audience, Name: strings.ToUpper(sub[:1]) + sub[1:], Email: sub + "@example.com"})
}

func (h *harness) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, h.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// route is one thing the registrar serves, and what it asks the store.
type route struct {
	method, path, body string
	relation, object   string
}

// routes is everything the registrar serves. The relation and the object
// beside each are the ones the director asked while it served the same path;
// TestEveryRouteAsksWhatTheDirectorAsked holds the two together.
func routes() []route {
	tenant, cluster := "tenant:demo", "cluster:"+dt.Cluster
	c := "/v1/clusters/" + dt.Cluster
	return []route{
		{"GET", "/v1/tenants/demo/people", "", "can_manage_users", tenant},
		{"GET", "/v1/tenants/demo/people/u1", "", "can_manage_users", tenant},
		{"GET", "/v1/tenants/demo/groups", "", "can_manage_users", tenant},
		{"GET", "/v1/tenants/demo/identity", "", "can_manage_users", tenant},
		{"GET", "/v1/tenants/demo/group-members?group=gentian:tenant:demo:members", "", "can_manage_users", tenant},
		{"GET", "/v1/tenants/demo/templates", "", "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/invite-person", `{"email":"ada@example.com"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/set-membership", `{"person":"u1","group":"gentian:tenant:demo:members","member":true}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/send-password-reset", `{"person":"u1"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/update-person", `{"person":"u1"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/remove-person", `{"person":"u1"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/require-totp", `{"person":"u1"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/remove-totp", `{"person":"u1"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/create-group", `{"name":"sales"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/delete-group", `{"group":"gentian:tenant:demo:sales"}`, "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/rename-group", `{"group":"gentian:tenant:demo:sales","name":"field"}`, "can_manage_users", tenant},
		{"GET", "/v1/tenants/demo/removed-mailboxes", "", "can_manage_users", tenant},
		{"POST", "/v1/tenants/demo/actions/delete-archived-mailbox", `{"mailbox":"demo-arch1"}`, "can_manage_users", tenant},
		{"POST", c + "/tenants/demo/actions/activate-admin", "", "can_configure", cluster},
		{"GET", c + "/people/count", "", "can_audit", cluster},
	}
}

// Each route asks the store the question written beside it: the same
// relation on the same object as when the director served the path. Moving
// the routes must not have widened or narrowed any.
//
// A write asks one question more, and always the same one: whether the
// caller may approve the tenant's public addresses. It opens nothing -- the
// first question has been answered by then -- and decides only whether the
// write may change who approves them (holdApprovers).
func TestEveryRouteAsksWhatTheDirectorAsked(t *testing.T) {
	f := newFakeIdentity("demo", "solo", "other")
	h := startWithIdentity(t, f)
	alice := h.token(t, "gentian", "alice")
	for _, rt := range routes() {
		before := len(h.asked.questions())
		status, body := h.do(t, rt.method, rt.path, alice, rt.body)
		if status >= 400 {
			t.Errorf("%s %s: alice was answered %d %v", rt.method, rt.path, status, body)
		}
		got := h.asked.questions()[before:]
		want := []string{"user:alice " + rt.relation + " " + rt.object}
		if rt.method != "GET" {
			want = append(want, "user:alice can_expose tenant:demo")
		}
		if strings.Join(got, ", ") != strings.Join(want, ", ") {
			t.Errorf("%s %s asked %v, want exactly %v", rt.method, rt.path, got, want)
		}
	}
}

// Nothing here is served to a caller who cannot be identified, and nothing
// reaches a realm for them.
func TestNoTokenNoRoute(t *testing.T) {
	f := newFakeIdentity("demo", "solo", "other")
	h := startWithIdentity(t, f)
	for name, tok := range map[string]string{
		"no token":       "",
		"garbage":        "abc",
		"wrong audience": h.issuer.Token(t, dt.Claims{Realm: "tenant-demo", Subject: "tom", Audience: "nextcloud"}),
		"foreign issuer": dt.NewIssuer(t, "tenant-demo").Token(t, dt.Claims{Realm: "tenant-demo", Subject: "tom", Audience: audience}),
		"forged key":     h.issuer.Token(t, dt.Claims{Realm: "tenant-demo", Subject: "tom", Audience: audience, Key: dt.OtherKey(t)}),
	} {
		for _, rt := range routes() {
			if status, _ := h.do(t, rt.method, rt.path, tok, rt.body); status != http.StatusUnauthorized {
				t.Errorf("%s: %s %s answered %d, want 401", name, rt.method, rt.path, status)
			}
		}
	}
	if seen := f.realmsSeen(); len(seen) != 0 {
		t.Fatalf("an unauthenticated request reached %v", seen)
	}
	if q := h.asked.questions(); len(q) != 0 {
		t.Fatalf("the store was asked about nobody: %v", q)
	}
}

// A member of the tenant holds none of these relations, and is refused every
// route before anything reaches the realm.
func TestSomebodyWithoutTheRelationIsRefusedEveryRoute(t *testing.T) {
	f := newFakeIdentity("demo", "solo", "other")
	h := startWithIdentity(t, f)
	mia := h.token(t, "tenant-demo", "mia")
	for _, rt := range routes() {
		if status, _ := h.do(t, rt.method, rt.path, mia, rt.body); status != http.StatusForbidden {
			t.Errorf("%s %s answered mia %d, want 403", rt.method, rt.path, status)
		}
	}
	if seen := f.realmsSeen(); len(seen) != 0 {
		t.Fatalf("a refused call still reached %v", seen)
	}
}

type failing struct{}

func (failing) Check(context.Context, string, string, string, string) (bool, error) {
	return false, errors.New("connection refused")
}

// A store that does not answer allows nothing.
func TestAnUnreachableDecisionPointDenies(t *testing.T) {
	f := newFakeIdentity("demo", "solo", "other")
	h := startWith(t, f, failing{})
	tom := h.token(t, "tenant-demo", "tom")
	for _, rt := range routes() {
		if status, _ := h.do(t, rt.method, rt.path, tom, rt.body); status != http.StatusServiceUnavailable {
			t.Errorf("%s %s answered %d, want 503", rt.method, rt.path, status)
		}
	}
	if seen := f.realmsSeen(); len(seen) != 0 {
		t.Fatalf("a call went through without a decision: %v", seen)
	}
}

func TestATenantTheClusterDoesNotHaveIsNotFound(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	alice := h.token(t, "gentian", "alice")
	path := fmt.Sprintf("/v1/clusters/%s/tenants/nowhere/actions/activate-admin", dt.Cluster)
	if status, _ := h.do(t, http.MethodPost, path, alice, ""); status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
	if status, _ := h.do(t, http.MethodGet, "/v1/tenants/Demo/people", alice, ""); status != http.StatusBadRequest {
		t.Fatalf("a name that is not a tenant name: %d, want 400", status)
	}
	other := "/v1/clusters/another-cluster/people/count"
	if status, _ := h.do(t, http.MethodGet, other, alice, ""); status != http.StatusBadRequest {
		t.Fatalf("another cluster's id: %d, want 400", status)
	}
}
