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
	"strings"
	"sync"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// fakeIdentity records which realm every call was made against. That is what
// these tests are really about: the caller is checked against a tenant, and
// the realm the registrar then reaches must be that tenant's and no other.
type fakeIdentity struct {
	mu sync.Mutex
	// held is the set of realms this registrar has a credential for.
	held map[string]bool
	// seen is every realm an operation was performed against, in order.
	seen []string
	// groups is what each realm has.
	groups map[string][]identity.Group
	// requestIDs is what travelled with each call.
	requestIDs []string
	// lastInvite is what the last invitation asked for.
	lastInvite identity.Invitation
	// landing is what the zone client says its root is.
	landing string
	// lastGroup is the path the last group operation named.
	lastGroup string
	// removed is every person removed.
	removed []string
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
		BaseURL:            "https://unused.invalid",
		Source:             identity.StaticSource{name: {Realm: name, ClientID: "x", ClientSecret: "y"}},
		PlatformRoleGroups: func(context.Context) ([]string, error) { return []string{platformAdmins}, nil },
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

func (f *fakeIdentity) SendPasswordReset(ctx context.Context, r identity.Realm, userID, _, _ string) error {
	f.note(ctx, r)
	if userID != "u1" {
		return identity.ErrNotFound
	}
	return nil
}

func (f *fakeIdentity) ZoneLanding(_ context.Context, r identity.Realm, clientID string) string {
	if f.landing == "" {
		return ""
	}
	return f.landing
}

var errNotAnAddress = errNotAddress{}

type errNotAddress struct{}

func (errNotAddress) Error() string { return `not an address: ""` }

// The property everything else rests on: the realm the registrar reaches is
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

// A realm this registrar was given no credential for is the PLATFORM's
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
// registrar's record of the authority. It has to be on the call.
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
	// A realm whose client states no root gets no redirect, which is the old
	// behaviour rather than a failure: sending one Keycloak refuses would make
	// every invitation fail.
	if f.lastInvite.RedirectURI != "" {
		t.Fatalf("redirect = %q, want none when the client states no root", f.lastInvite.RedirectURI)
	}
}

// The redirect comes off the zone client, not from a second derivation of the
// zone's domain here. The domain is a vanity name, or the kernel domain under
// single tenancy, or the tenant's subdomain -- and the answer that disagreed
// with the Gateway would be refused by Keycloak on a page that says nothing
// about a redirect URI list.
func TestTheInvitationLandsWhereTheZoneClientSays(t *testing.T) {
	f := newFakeIdentity("demo")
	f.landing = "https://console.demo.example.test/"
	h := startWithIdentity(t, f)

	if status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/invite-person",
		h.token(t, "tenant-demo", "tom"), `{"email":"ada@example.com"}`); status != http.StatusAccepted {
		t.Fatalf("invite: %d", status)
	}
	if f.lastInvite.RedirectURI != "https://console.demo.example.test/" {
		t.Fatalf("redirect = %q", f.lastInvite.RedirectURI)
	}
}

// The registrar serves the routes even before any credential has arrived.
//
// Gating registration on "are there credentials right now" fails exactly the
// case that has to work: on a first install the operator writes the Secret
// minutes after the registrar starts, and the screens would never appear no
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

// An administrator acting for somebody who cannot act for themselves.
//
// The self-service half is deliberately absent: a locked-out person holds no
// token, so there is no caller for OpenFGA to answer about, and an endpoint
// that skipped the check would be the one unauthenticated write into a source
// of truth. Keycloak's own login page sends that mail.
func TestAnAdministratorCanSendAPasswordReset(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/send-password-reset",
		h.token(t, "tenant-demo", "tom"), `{"person":"u1"}`)
	if status != http.StatusAccepted {
		t.Fatalf("status %d %v", status, body)
	}
	if body["mailed"] != true {
		t.Errorf("body = %v", body)
	}
	if seen := f.realmsSeen(); len(seen) == 0 || seen[len(seen)-1] != "demo" {
		t.Errorf("realms reached: %v", seen)
	}
}

func TestAMemberCannotSendSomebodyElseAReset(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)

	status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/send-password-reset",
		h.token(t, "tenant-demo", "mia"), `{"person":"u1"}`)
	if status != http.StatusForbidden {
		t.Fatalf("status %d, want 403", status)
	}
}

func (f *fakeIdentity) UpdatePerson(ctx context.Context, r identity.Realm, id string, _ identity.PersonUpdate) (identity.Person, error) {
	f.note(ctx, r)
	return identity.Person{ID: id}, nil
}

func (f *fakeIdentity) RemovePerson(ctx context.Context, r identity.Realm, id string) error {
	f.note(ctx, r)
	f.mu.Lock()
	f.removed = append(f.removed, id)
	f.mu.Unlock()
	return nil
}

func (f *fakeIdentity) RequireTOTP(ctx context.Context, r identity.Realm, _ string, _ bool, _, _ string) error {
	f.note(ctx, r)
	return nil
}

func (f *fakeIdentity) UserCount(ctx context.Context, r identity.Realm) (int, error) {
	f.note(ctx, r)
	return 7, nil
}

func (f *fakeIdentity) RemoveTOTP(ctx context.Context, r identity.Realm, _ string) error {
	f.note(ctx, r)
	return nil
}

func (f *fakeIdentity) CreateGroup(ctx context.Context, r identity.Realm, path string) (identity.Group, error) {
	f.note(ctx, r)
	f.mu.Lock()
	f.lastGroup = path
	f.mu.Unlock()
	return identity.Group{ID: "g-new", Path: path, Name: path, Custom: true}, nil
}

func (f *fakeIdentity) DeleteGroup(ctx context.Context, r identity.Realm, path string) error {
	f.note(ctx, r)
	f.mu.Lock()
	f.lastGroup = path
	f.mu.Unlock()
	return nil
}

func (f *fakeIdentity) RenameGroup(ctx context.Context, r identity.Realm, _, newPath string) (identity.Group, error) {
	f.note(ctx, r)
	f.mu.Lock()
	f.lastGroup = newPath
	f.mu.Unlock()
	return identity.Group{ID: "g", Path: newPath, Name: newPath, Custom: true}, nil
}

func (f *fakeIdentity) FindUser(ctx context.Context, r identity.Realm, username string) (identity.Person, error) {
	f.note(ctx, r)
	return identity.Person{ID: "admin-id", Username: username}, nil
}

func (f *fakeIdentity) ActivateAccount(ctx context.Context, r identity.Realm, id, email string, requireMFA bool, _, _ string) (identity.Activation, error) {
	f.note(ctx, r)
	actions := []string{"UPDATE_PASSWORD"}
	if requireMFA {
		actions = append(actions, "CONFIGURE_TOTP")
	}
	if email != "" {
		return identity.Activation{Mailed: true, Email: email, Actions: actions}, nil
	}
	return identity.Activation{Link: "https://id.example/link?key=" + id, ExpiresAt: 1, Actions: actions}, nil
}

func (f *fakeIdentity) GroupMembers(ctx context.Context, r identity.Realm, _ string) ([]identity.Person, error) {
	f.note(ctx, r)
	return nil, nil
}

// The form sends the part before the @; the registrar composes the login under
// the tenant's own domain, and reports that domain for the form to show.
func TestInviteComposesTheLoginUnderTheTenantsDomain(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tok := h.token(t, "tenant-demo", "tom")

	status, settings := h.do(t, http.MethodGet, "/v1/tenants/demo/identity", tok, "")
	if status != http.StatusOK {
		t.Fatalf("identity: %d %v", status, settings)
	}
	domain, _ := settings["loginDomain"].(string)
	if !strings.HasPrefix(domain, "demo.") {
		t.Fatalf("a tenant with its own realm signs in under <tenant>.<kernel>: %q", domain)
	}

	status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/invite-person", tok,
		`{"email":"jane@example.org","username":"Jane-Doe","firstName":"Jane","lastName":"Doe","requireTotp":true}`)
	if status != http.StatusAccepted {
		t.Fatalf("invite: %d %v", status, body)
	}
	f.mu.Lock()
	inv := f.lastInvite
	f.mu.Unlock()
	if inv.Username != "jane-doe@"+domain || inv.Email != "jane@example.org" ||
		inv.FirstName != "Jane" || inv.LastName != "Doe" || !inv.RequireTOTP {
		t.Fatalf("invitation: %+v", inv)
	}

	status, _ = h.do(t, http.MethodPost, "/v1/tenants/demo/actions/invite-person", tok,
		`{"email":"x@example.org","username":"not/a/name"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("a username with a slash: %d, want 400", status)
	}
}

// A custom group is made inside the tenant's subtree from a label, and a label
// cannot name one of the platform's own groups or reach past a colon.
func TestCustomGroupsLiveInTheTenantsSubtree(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tok := h.token(t, "tenant-demo", "tom")

	status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/create-group", tok, `{"name":"Sales"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	f.mu.Lock()
	got := f.lastGroup
	f.mu.Unlock()
	if got != "gentian:tenant:demo:sales" {
		t.Fatalf("created %q", got)
	}
	for _, bad := range []string{`{"name":"admin"}`, `{"name":"app:x"}`, `{"name":""}`} {
		if status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/create-group", tok, bad); status != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, status)
		}
	}
}

// Nobody removes themselves from here.
func TestRemovingYourselfIsRefused(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tok := h.token(t, "tenant-demo", "tom")

	status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tok, `{"person":"tom"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("removing yourself: %d, want 400", status)
	}
	status, _ = h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tok, `{"person":"u1"}`)
	if status != http.StatusOK {
		t.Fatalf("removing somebody else: %d", status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.removed) != 1 || f.removed[0] != "u1" {
		t.Fatalf("removed %v", f.removed)
	}
}

func TestRenamingAGroupKeepsItInTheTenantsSubtree(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tok := h.token(t, "tenant-demo", "tom")

	status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/rename-group", tok,
		`{"group":"gentian:tenant:demo:sales","name":"Field-Sales"}`)
	if status != http.StatusOK {
		t.Fatalf("rename: %d %v", status, body)
	}
	f.mu.Lock()
	got := f.lastGroup
	f.mu.Unlock()
	if got != "gentian:tenant:demo:field-sales" {
		t.Fatalf("renamed to %q", got)
	}
	if status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/rename-group", tok,
		`{"group":"gentian:tenant:demo:sales","name":"admin"}`); status != http.StatusBadRequest {
		t.Fatalf("renaming onto a platform name: %d, want 400", status)
	}
}

// Whoever may bring tenants on hands the administrator account over: a link to
// show when no address is given, a mail when one is. A tenant administrator
// cannot issue it for their own account.
func TestActivatingATenantAdministrator(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	path := "/v1/clusters/" + dt.Cluster + "/tenants/demo/actions/activate-admin"
	alice := h.token(t, "gentian", "alice")

	status, body := h.do(t, http.MethodPost, path, alice, "")
	if status != http.StatusOK {
		t.Fatalf("activate: %d %v", status, body)
	}
	activation, _ := body["activation"].(map[string]any)
	if activation["link"] == nil || activation["mailed"] == true {
		t.Fatalf("without an address the link comes back: %v", body)
	}
	if u, _ := body["username"].(string); !strings.HasPrefix(u, "admin@demo.") {
		t.Fatalf("username %q", u)
	}
	actions, _ := activation["actions"].([]any)
	if len(actions) != 2 {
		t.Fatalf("a second factor is required by default: %v", actions)
	}

	status, body = h.do(t, http.MethodPost, path, alice, `{"recoveryEmail":"owner@example.org"}`)
	activation, _ = body["activation"].(map[string]any)
	if status != http.StatusOK || activation["mailed"] != true || activation["link"] != nil {
		t.Fatalf("with an address it is mailed: %d %v", status, body)
	}

	if status, _ := h.do(t, http.MethodPost, path, h.token(t, "tenant-demo", "tom"), ""); status != http.StatusForbidden {
		t.Fatalf("a tenant administrator issuing it: %d, want 403", status)
	}
}

// The cluster's count is per realm, and a realm the registrar cannot reach
// makes the total incomplete instead of smaller.
func TestTheClusterCountsItsPeoplePerRealm(t *testing.T) {
	f := newFakeIdentity("demo", "solo") // other deliberately absent
	h := startWithIdentity(t, f)
	path := "/v1/clusters/" + dt.Cluster + "/people/count"

	status, body := h.do(t, http.MethodGet, path, h.token(t, "gentian", "alice"), "")
	if status != http.StatusOK {
		t.Fatalf("count: %d %v", status, body)
	}
	if body["users"] != float64(14) || body["complete"] != false {
		t.Fatalf("two realms of seven and one unreachable: %v", body)
	}
	for _, raw := range body["realms"].([]any) {
		realm := raw.(map[string]any)
		_, counted := realm["users"]
		if want := realm["realm"] != "other"; counted != want {
			t.Errorf("realm %v counted=%v, want %v", realm["realm"], counted, want)
		}
	}

	if status, _ := h.do(t, http.MethodGet, path, h.token(t, "tenant-demo", "tom"), ""); status != http.StatusForbidden {
		t.Fatalf("a tenant administrator counting the cluster: %d, want 403", status)
	}
}
