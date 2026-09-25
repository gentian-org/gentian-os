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

package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/entitlement"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/identity"
)

// fakeIdentity records which realm every call was made against. That is what
// these tests are really about: the caller is checked against a tenant, and
// the realm the director then reaches must be that tenant's and no other.
type fakeIdentity struct {
	mu sync.Mutex
	// held is the set of realms this director has a credential for.
	held map[string]bool
	// seen is every realm an operation was performed against, in order.
	seen []string
	// groups is what each realm has.
	groups map[string][]identity.Group
	// requestIDs is what travelled with each call.
	requestIDs []string
	// lastInvite is what the last invitation asked for.
	lastInvite identity.Invitation
}

func newFakeIdentity(realms ...string) *fakeIdentity {
	f := &fakeIdentity{held: map[string]bool{}, groups: map[string][]identity.Group{}}
	for _, r := range realms {
		f.held[r] = true
	}
	return f
}

func (f *fakeIdentity) Realm(name string) (identity.Realm, error) {
	if !f.held[name] {
		return identity.Realm{}, identity.ErrNoCredential
	}
	// The real client hands back an opaque token; a fake cannot construct one
	// with the unexported field set, so it uses the real constructor.
	c, err := identity.New(identity.Config{
		BaseURL: "https://unused.invalid",
		Source:  identity.StaticSource{name: {Realm: name, ClientID: "x", ClientSecret: "y"}},
	})
	if err != nil {
		return identity.Realm{}, err
	}
	return c.Realm(name)
}

func (f *fakeIdentity) note(ctx context.Context, r identity.Realm) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Name())
	f.requestIDs = append(f.requestIDs, identity.RequestIDFrom(ctx))
}

func (f *fakeIdentity) realmsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeIdentity) People(ctx context.Context, r identity.Realm, _ string, _ int) ([]identity.Person, error) {
	f.note(ctx, r)
	return []identity.Person{{ID: "u1", Username: "ada@example.com", Email: "ada@example.com", Enabled: true}}, nil
}

func (f *fakeIdentity) Person(ctx context.Context, r identity.Realm, id string) (identity.Person, error) {
	f.note(ctx, r)
	if id != "u1" {
		return identity.Person{}, identity.ErrNotFound
	}
	return identity.Person{ID: "u1", Username: "ada@example.com"}, nil
}

func (f *fakeIdentity) Groups(ctx context.Context, r identity.Realm) ([]identity.Group, error) {
	f.note(ctx, r)
	return f.groups[r.Name()], nil
}

func (f *fakeIdentity) Invite(ctx context.Context, r identity.Realm, inv identity.Invitation) (identity.Person, error) {
	f.note(ctx, r)
	f.mu.Lock()
	f.lastInvite = inv
	f.mu.Unlock()
	if !strings.Contains(inv.Email, "@") {
		return identity.Person{}, errNotAnAddress
	}
	if inv.Email == "taken@example.com" {
		return identity.Person{}, identity.ErrConflict
	}
	return identity.Person{ID: "new", Username: inv.Email, Email: inv.Email, Pending: true}, nil
}

func (f *fakeIdentity) SetMembership(ctx context.Context, r identity.Realm, _, group string, _ bool) error {
	f.note(ctx, r)
	if strings.HasPrefix(group, "gentian:platform:") {
		return identity.ErrOutOfScope
	}
	return nil
}

func (f *fakeIdentity) PasswordPolicy(ctx context.Context, r identity.Realm) (string, error) {
	f.note(ctx, r)
	return "length(8)", nil
}

func (f *fakeIdentity) SetPasswordPolicy(ctx context.Context, r identity.Realm, _ string) error {
	f.note(ctx, r)
	return nil
}

var errNotAnAddress = errNotAddress{}

type errNotAddress struct{}

func (errNotAddress) Error() string { return `not an address: ""` }

// startWithIdentity is the harness with a director that speaks for realms.
func startWithIdentity(t *testing.T, ident api.Identity) *harness {
	t.Helper()
	is := dt.NewIssuer(t, "gentian", "tenant-demo", "tenant-solo")
	v, err := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	remote := dt.Remote(t, "demo", "solo", "other")
	repo := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, gitops.Person{})
	decisions, tuples := checker(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	verifier, err := entitlement.NewVerifier(map[string]ed25519.PublicKey{"store-1": pub}, dt.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(api.Config{
		Authn: v, Authz: decisions, Viewer: fixedViewer{}, Repo: repo, Cluster: dt.Cluster,
		Store:    &api.StoreConfig{Verifier: verifier, Applier: &entitlement.Applier{Repo: repo, Store: tuples}},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Identity: ident,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{Server: httptest.NewServer(srv), issuer: is, remote: remote, storeKey: priv}
	t.Cleanup(h.Close)
	return h
}

// The property everything else rests on: the realm the director reaches is
// the realm of the tenant the caller was just checked against.
func TestPeopleReachOnlyTheTenantsOwnRealm(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	h := startWithIdentity(t, f)

	status, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/people", h.token(t, "tenant-demo", "tom"), "")
	if status != http.StatusOK {
		t.Fatalf("tom listing demo: %d", status)
	}
	seen := f.realmsSeen()
	if len(seen) != 1 || seen[0] != "demo" {
		t.Fatalf("realms reached: %v, want [demo]", seen)
	}
}

// tina administers solo and nothing in demo. The refusal has to happen before
// anything reaches a realm.
func TestAnotherTenantsAdministratorIsRefusedBeforeTheRealm(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	h := startWithIdentity(t, f)

	status, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/people", h.token(t, "tenant-solo", "tina"), "")
	if status != http.StatusForbidden {
		t.Fatalf("tina listing demo: %d, want 403", status)
	}
	if seen := f.realmsSeen(); len(seen) != 0 {
		t.Fatalf("a refused call still reached %v", seen)
	}
}

// A member is not an administrator. can_manage_users is admin-only, and
// reading a tenant's people is not covered by can_view.
func TestAMemberMayNotListThePeople(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	status, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/people", h.token(t, "tenant-demo", "mia"), "")
	if status != http.StatusForbidden {
		t.Fatalf("mia listing demo: %d, want 403", status)
	}
}

// A realm this director was given no credential for is the PLATFORM's
// problem, not the caller's. 403 would send them to ask for a permission they
// already hold.
func TestARealmWithNoCredentialIs503NotForbidden(t *testing.T) {
	f := newFakeIdentity("solo") // demo deliberately absent
	h := startWithIdentity(t, f)

	status, body := h.do(t, http.MethodGet, "/v1/tenants/demo/people", h.token(t, "tenant-demo", "tom"), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", status)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "demo") {
		t.Errorf("the refusal should name the realm: %v", body)
	}
}

func TestInviteIsAnActionAndReportsWhoWasCreated(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tok := h.token(t, "tenant-demo", "tom")

	status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/invite-person", tok,
		`{"email":"ada@example.com","groups":["gentian:tenant:demo:members"]}`)
	if status != http.StatusAccepted {
		t.Fatalf("invite: %d %v", status, body)
	}
	person, _ := body["person"].(map[string]any)
	if person["email"] != "ada@example.com" || person["pending"] != true {
		t.Fatalf("person = %v", person)
	}

	// There is no PUT. A person is not declared state and does not belong in
	// an append-only history, which is what a commit route would make them.
	status, _ = h.do(t, http.MethodPut, "/v1/tenants/demo/people", tok, `{}`)
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /people answered %d; it should not exist", status)
	}
}

func TestInvitingSomebodyAlreadyThereIsAConflict(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/invite-person",
		h.token(t, "tenant-demo", "tom"), `{"email":"taken@example.com"}`)
	if status != http.StatusConflict {
		t.Fatalf("status %d, want 409", status)
	}
}

// A group outside what this tenant may touch is a bad request, not a refusal:
// the caller holds the relation, and "forbidden" would send them to ask for a
// permission that would not help.
func TestAGroupOutsideTheTenantIsABadRequest(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/set-membership",
		h.token(t, "tenant-demo", "tom"),
		`{"person":"u1","group":"gentian:platform:admin","member":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", status)
	}
}

// Membership has to say which way. Without it the request is ambiguous and
// defaulting either way is a change nobody asked for.
func TestSetMembershipRequiresSayingWhich(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/set-membership",
		h.token(t, "tenant-demo", "tom"), `{"person":"u1","group":"gentian:tenant:demo:members"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", status)
	}
}

func TestThePasswordPolicyIsReadAndWritten(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tok := h.token(t, "tenant-demo", "tom")

	status, body := h.do(t, http.MethodGet, "/v1/tenants/demo/identity", tok, "")
	if status != http.StatusOK || body["passwordPolicy"] != "length(8)" {
		t.Fatalf("read: %d %v", status, body)
	}
	status, _ = h.do(t, http.MethodPost, "/v1/tenants/demo/actions/set-password-policy", tok,
		`{"passwordPolicy":"length(12)"}`)
	if status != http.StatusOK {
		t.Fatalf("write: %d", status)
	}
}

// The request id is what joins Keycloak's record of the change to the
// director's record of the authority. It has to be on the call.
func TestTheRequestIdReachesTheRealm(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	if status, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/people",
		h.token(t, "tenant-demo", "tom"), ""); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requestIDs) != 1 || f.requestIDs[0] == "" {
		t.Fatalf("request ids = %v; every admin call carries one", f.requestIDs)
	}
}

// A director with no Keycloak credential at all serves none of these routes.
// A console then shows the screens as absent rather than as failing.
func TestWithoutIdentityTheScreensDoNotExist(t *testing.T) {
	h := start(t, false)
	for _, path := range []string{
		"/v1/tenants/demo/people",
		"/v1/tenants/demo/groups",
		"/v1/tenants/demo/identity",
	} {
		status, _ := h.do(t, http.MethodGet, path, h.token(t, "tenant-demo", "tom"), "")
		if status != http.StatusNotFound {
			t.Errorf("%s answered %d; with no credential it should not be registered", path, status)
		}
	}
}

// The invitation link has to name a client that exists in the realm, and the
// zone's own client is what the person signs in through the moment they have
// a password. Derived, because a value configured per tenant is one more
// thing to write per tenant and one more thing to get wrong.
func TestTheInvitationNamesTheZonesOwnClient(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	if status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/invite-person",
		h.token(t, "tenant-demo", "tom"), `{"email":"ada@example.com"}`); status != http.StatusAccepted {
		t.Fatalf("invite: %d", status)
	}
	if f.lastInvite.ClientID != "gentian-edge-demo" {
		t.Fatalf("client = %q, want gentian-edge-demo", f.lastInvite.ClientID)
	}
	// And no redirect: the zone client's valid redirect URIs are each host's
	// /oauth2/callback, which is not a page to land on. Sending one Keycloak
	// refuses would make every invitation fail.
	if f.lastInvite.RedirectURI != "" {
		t.Fatalf("redirect = %q, want none until the client accepts one", f.lastInvite.RedirectURI)
	}
}

// A director configured to speak for realms serves the routes even before any
// credential has arrived.
//
// Gating registration on "are there credentials right now" fails exactly the
// case that has to work: on a first install the operator writes the Secret
// minutes after the director starts, and the screens would never appear no
// matter how many realms arrived. A realm with no credential is a 503 naming
// it, which is a refusal somebody can act on; a route that does not exist is
// not.
func TestTheRoutesExistBeforeAnyCredentialArrives(t *testing.T) {
	f := newFakeIdentity() // configured, holding nothing
	h := startWithIdentity(t, f)

	status, body := h.do(t, http.MethodGet, "/v1/tenants/demo/people",
		h.token(t, "tenant-demo", "tom"), "")
	if status == http.StatusNotFound {
		t.Fatal("the route was not registered; a first install would never serve it")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", status)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "demo") {
		t.Errorf("the refusal should name the realm: %v", body)
	}
}
