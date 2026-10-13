/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
	"github.com/gentian-org/gentian-os/internal/registrar/record"
)

// The administrator of a tenant that does not exist yet is invited by the
// registrar, when the tenant is ready, without anybody keeping a page open.
// These run the routes and the pass the registrar makes over the waiting
// requests, with a clock that is moved by hand and the requests kept in
// memory; that the SQL keeps them is record's own test.

const owner = "owner@example.org"

// fakeClock is the time, moved by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lockedBuffer is what the registrar logged.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeInvitations keeps the requests and their outcomes as record.Store
// does: one request per tenant, and an outcome in its place.
type fakeInvitations struct {
	mu       sync.Mutex
	waiting  map[string]record.Invitation
	outcomes []record.Action
}

func newFakeInvitations() *fakeInvitations {
	return &fakeInvitations{waiting: map[string]record.Invitation{}}
}

func (f *fakeInvitations) RequestInvitation(_ context.Context, inv record.Invitation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waiting[inv.Tenant] = inv
	return nil
}

func (f *fakeInvitations) PendingInvitations(context.Context) ([]record.Invitation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]record.Invitation, 0, len(f.waiting))
	for _, inv := range f.waiting {
		out = append(out, inv)
	}
	return out, nil
}

func (f *fakeInvitations) AttemptInvitation(_ context.Context, tenant string, attempt func(record.Invitation) *record.Action) error {
	f.mu.Lock()
	inv, ok := f.waiting[tenant]
	f.mu.Unlock()
	if !ok {
		return nil
	}
	outcome := attempt(inv)
	if outcome == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.waiting, tenant)
	f.outcomes = append(f.outcomes, *outcome)
	return nil
}

func (f *fakeInvitations) InvitationState(_ context.Context, tenant string) (record.InvitationState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inv, ok := f.waiting[tenant]; ok {
		return record.InvitationState{State: record.InvitationPending, At: inv.RequestedAt, ExpiresAt: inv.ExpiresAt}, nil
	}
	for i := len(f.outcomes) - 1; i >= 0; i-- {
		if o := f.outcomes[i]; o.Tenant == tenant {
			state, _ := record.OutcomeState(o.Action)
			return record.InvitationState{State: state, At: o.At, Reason: o.Detail}, nil
		}
	}
	return record.InvitationState{State: record.InvitationNone}, nil
}

func (f *fakeInvitations) request(tenant string) (record.Invitation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.waiting[tenant]
	return inv, ok
}

func (f *fakeInvitations) outcome(t *testing.T, tenant string) record.Action {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []record.Action
	for _, o := range f.outcomes {
		if o.Tenant == tenant {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("tenant %s has %d outcomes, want one: %+v", tenant, len(found), found)
	}
	return found[0]
}

// arriving is a cluster whose tenants, realm credentials and administrator
// accounts arrive while the registrar runs, as a new tenant's do.
type arriving struct {
	*fakeIdentity
	mu       sync.Mutex
	tenants  map[string]registrar.Tenant
	realms   map[string]bool
	accounts map[string]bool
	// mailed is the address of every activation that was asked for, and
	// requestIDs the request id each carried.
	mailed     []string
	requestIDs []string
}

func newArriving() *arriving {
	return &arriving{
		fakeIdentity: newFakeIdentity(),
		tenants:      map[string]registrar.Tenant{},
		realms:       map[string]bool{},
		accounts:     map[string]bool{},
	}
}

func (a *arriving) Tenant(_ context.Context, name string) (registrar.Tenant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tenants[name]
	if !ok {
		return registrar.Tenant{}, registrar.ErrTenantNotFound
	}
	return t, nil
}

func (a *arriving) All(context.Context) ([]registrar.Tenant, error) { return nil, nil }

func (a *arriving) Realm(name string) (identity.Realm, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.realms[name] {
		return identity.Realm{}, identity.ErrNoCredential
	}
	return newFakeIdentity(name).Realm(name)
}

func (a *arriving) FindUser(_ context.Context, _ identity.Realm, username string) (identity.Person, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.accounts[username] {
		return identity.Person{}, identity.ErrNotFound
	}
	return identity.Person{ID: "admin-id", Username: username}, nil
}

func (a *arriving) ActivateAccount(ctx context.Context, _ identity.Realm, _, email string, _ bool, _, _ string) (identity.Activation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mailed = append(a.mailed, email)
	a.requestIDs = append(a.requestIDs, identity.RequestIDFrom(ctx))
	return identity.Activation{Mailed: true, Email: email, Actions: []string{"UPDATE_PASSWORD"}}, nil
}

func (a *arriving) set(change func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change()
}

func (a *arriving) sent() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.mailed...)
}

func startArriving(t *testing.T) (*harness, *arriving) {
	t.Helper()
	a := newArriving()
	return startWithTenants(t, a, nil, a, newFakeMailboxes()), a
}

func invitationPath(tenant string) string {
	return "/v1/clusters/" + dt.Cluster + "/tenants/" + tenant + "/actions/request-admin-invitation"
}

func (h *harness) invitationState(t *testing.T, tenant string) map[string]any {
	t.Helper()
	status, body := h.do(t, http.MethodGet, "/v1/clusters/"+dt.Cluster+"/tenants/"+tenant+"/admin-invitation",
		h.token(t, "gentian", "alice"), "")
	if status != http.StatusOK {
		t.Fatalf("the invitation's state: %d %v", status, body)
	}
	return body
}

// nothingNames fails if text carries any of what must not be written down.
func nothingNames(t *testing.T, what, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(strings.ToLower(text), strings.ToLower(secret)) {
			t.Errorf("%s names %q:\n%s", what, secret, text)
		}
	}
}

// A request for a tenant that does not exist is accepted at once and kept:
// the tenant, the address, who asked, what permitted it and when. What is
// answered about it afterwards is that it waits, and never where it goes.
func TestAnInvitationForATenantThatDoesNotExistIsAcceptedAndKept(t *testing.T) {
	h, a := startArriving(t)
	alice := h.token(t, "gentian", "alice")

	status, body := h.do(t, http.MethodPost, invitationPath("newco"), alice, `{"recoveryEmail":" Owner@Example.org "}`)
	if status != http.StatusAccepted || body["state"] != "pending" || body["tenant"] != "newco" {
		t.Fatalf("request: %d %v", status, body)
	}
	inv, ok := h.invitations.request("newco")
	if !ok {
		t.Fatal("the request was not kept")
	}
	if inv.Email != owner || inv.Principal != "alice" || inv.Decision != "can_configure cluster:"+dt.Cluster ||
		inv.RequestID == "" || !inv.RequestedAt.Equal(h.clock.Now()) ||
		!inv.ExpiresAt.Equal(h.clock.Now().Add(registrar.DefaultInvitationExpiry)) {
		t.Fatalf("kept as %+v", inv)
	}
	if got := body["expiresAt"]; got != float64(inv.ExpiresAt.Unix()) {
		t.Fatalf("the answer says it expires at %v, kept until %v", got, inv.ExpiresAt.Unix())
	}

	state := h.invitationState(t, "newco")
	if state["state"] != "pending" {
		t.Fatalf("state: %v", state)
	}
	_, list := h.do(t, http.MethodGet, "/v1/clusters/"+dt.Cluster+"/admin-invitations", alice, "")
	pending, _ := list["pending"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["tenant"] != "newco" {
		t.Fatalf("the waiting invitations: %v", list)
	}
	for what, answer := range map[string]map[string]any{"the answer": body, "the state": state, "the list": list} {
		raw, _ := json.Marshal(answer)
		nothingNames(t, what, string(raw), owner)
	}
	if sent := a.sent(); len(sent) != 0 {
		t.Fatalf("nothing can have been mailed yet: %v", sent)
	}
}

// The invitation goes out once the activation is possible, and not before:
// the tenant, then the credential for its realm, then its administrator
// account. It is the same activation the route performs, with the address,
// for the person who asked; the request is gone afterwards and the outcome
// is recorded in its place.
func TestTheInvitationIsSentWhenTheTenantBecomesReady(t *testing.T) {
	h, a := startArriving(t)
	ctx := context.Background()
	status, body := h.do(t, http.MethodPost, invitationPath("newco"), h.token(t, "gentian", "alice"),
		`{"recoveryEmail":"`+owner+`"}`)
	if status != http.StatusAccepted {
		t.Fatalf("request: %d %v", status, body)
	}
	kept, _ := h.invitations.request("newco")
	login := "admin@newco." + dt.KernelDomain

	for _, step := range []struct {
		what    string
		arrives func()
	}{
		{"before the tenant exists", func() {}},
		{"with the tenant and no credential for its realm", func() { a.tenants["newco"] = ownRealm("newco") }},
		{"with the credential and no administrator account", func() { a.realms["newco"] = true }},
	} {
		a.set(step.arrives)
		h.clock.advance(time.Minute)
		h.registrar.SendInvitations(ctx)
		if sent := a.sent(); len(sent) != 0 {
			t.Fatalf("%s: mailed %v", step.what, sent)
		}
		if state := h.invitationState(t, "newco"); state["state"] != "pending" {
			t.Fatalf("%s: state %v", step.what, state)
		}
	}

	a.set(func() { a.accounts[login] = true })
	h.clock.advance(time.Minute)
	h.registrar.SendInvitations(ctx)
	if sent := a.sent(); len(sent) != 1 || sent[0] != owner {
		t.Fatalf("mailed to %v, want the address that was given", sent)
	}
	if a.requestIDs[0] != kept.RequestID {
		t.Fatalf("Keycloak's event carries %q, the request was %q", a.requestIDs[0], kept.RequestID)
	}
	if _, still := h.invitations.request("newco"); still {
		t.Fatal("the request is still kept after it was sent")
	}
	o := h.invitations.outcome(t, "newco")
	if o.Action != record.ActionInvitationSent || o.Target != login || o.Realm != "newco" ||
		o.Principal != "alice" || o.Decision != kept.Decision || o.RequestID != kept.RequestID {
		t.Fatalf("recorded as %+v", o)
	}
	if state := h.invitationState(t, "newco"); state["state"] != "sent" {
		t.Fatalf("state: %v", state)
	}

	// Once, and not again at the next round.
	h.registrar.SendInvitations(ctx)
	if sent := a.sent(); len(sent) != 1 {
		t.Fatalf("mailed %d times", len(sent))
	}
	nothingNames(t, "the log", h.logged.String(), owner)
	if !strings.Contains(h.logged.String(), record.ActionInvitationSent) {
		t.Fatalf("the outcome is not in the log:\n%s", h.logged.String())
	}
}

// One request per tenant: a second replaces the first, and the invitation
// goes to the second address only.
func TestASecondRequestReplacesTheFirst(t *testing.T) {
	h, a := startArriving(t)
	alice := h.token(t, "gentian", "alice")
	for _, address := range []string{owner, "second@example.org"} {
		h.clock.advance(time.Hour)
		if status, body := h.do(t, http.MethodPost, invitationPath("newco"), alice,
			`{"recoveryEmail":"`+address+`"}`); status != http.StatusAccepted {
			t.Fatalf("request for %s: %d %v", address, status, body)
		}
	}
	waiting, _ := h.invitations.PendingInvitations(context.Background())
	if len(waiting) != 1 || waiting[0].Email != "second@example.org" ||
		!waiting[0].ExpiresAt.Equal(h.clock.Now().Add(registrar.DefaultInvitationExpiry)) {
		t.Fatalf("kept: %+v", waiting)
	}

	a.set(func() {
		a.tenants["newco"] = ownRealm("newco")
		a.realms["newco"] = true
		a.accounts["admin@newco."+dt.KernelDomain] = true
	})
	h.registrar.SendInvitations(context.Background())
	if sent := a.sent(); len(sent) != 1 || sent[0] != "second@example.org" {
		t.Fatalf("mailed to %v", sent)
	}
}

// A tenant that never becomes ready: the request is given up when its time is
// over, deleted, and recorded as expired. Nothing is mailed, then or later.
func TestARequestExpires(t *testing.T) {
	h, a := startArriving(t)
	ctx := context.Background()
	if status, body := h.do(t, http.MethodPost, invitationPath("never"), h.token(t, "gentian", "alice"),
		`{"recoveryEmail":"`+owner+`"}`); status != http.StatusAccepted {
		t.Fatalf("request: %d %v", status, body)
	}
	h.clock.advance(registrar.DefaultInvitationExpiry - time.Second)
	h.registrar.SendInvitations(ctx)
	if _, waiting := h.invitations.request("never"); !waiting {
		t.Fatal("given up before its time")
	}

	h.clock.advance(time.Second)
	// Even a tenant that is ready by now: the request's time is over.
	a.set(func() {
		a.tenants["never"] = ownRealm("never")
		a.realms["never"] = true
		a.accounts["admin@never."+dt.KernelDomain] = true
	})
	h.registrar.SendInvitations(ctx)
	if _, waiting := h.invitations.request("never"); waiting {
		t.Fatal("the request is kept past its time")
	}
	if o := h.invitations.outcome(t, "never"); o.Action != record.ActionInvitationExpired || o.Principal != "alice" {
		t.Fatalf("recorded as %+v", o)
	}
	if state := h.invitationState(t, "never"); state["state"] != "expired" {
		t.Fatalf("state: %v", state)
	}
	h.registrar.SendInvitations(ctx)
	if sent := a.sent(); len(sent) != 0 {
		t.Fatalf("an expired request was mailed: %v", sent)
	}
	nothingNames(t, "the log", h.logged.String(), owner)
}

// startAgainstARealmThatCannotMail is the real Keycloak client behind the
// routes, against a realm that has the tenant's administrator and refuses to
// send mail, saying why the way Keycloak does.
func startAgainstARealmThatCannotMail(t *testing.T) (*harness, *realm) {
	t.Helper()
	k := &realm{}
	kc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/execute-actions-email"):
			k.mu.Lock()
			k.writes = append(k.writes, r.Method+" "+r.URL.Path)
			k.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errorMessage":"Failed to send execute actions email: 535 Authentication failed for ` + owner + `"}`))
		case strings.Contains(r.URL.Path, "/gentian-activation/"):
			k.mu.Lock()
			k.writes = append(k.writes, r.Method+" "+r.URL.Path)
			k.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"link":"` + shownLink + `","expiresAt":1}`))
		default:
			k.serve(w, r)
		}
	}))
	t.Cleanup(kc.Close)
	client, err := identity.New(identity.Config{
		BaseURL:            kc.URL,
		Source:             identity.StaticSource{"demo": {Realm: "demo", ClientID: identity.ClientID, ClientSecret: "s"}},
		PlatformRoleGroups: func(context.Context) ([]string, error) { return []string{platformAdmins}, nil },
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return startWithIdentity(t, client), k
}

// shownLink is the link the realm would issue for somebody to be shown.
const shownLink = "https://id.example.org/realms/demo/login-actions/action-token?key=one-time-key"

// The tenant is ready and its realm cannot send mail. No link is issued,
// because there is nobody to show it to and nowhere it may be kept. The
// address stays on the account as its recovery address, as the route would
// leave it; the request is deleted and recorded as not mailed, with the
// reason the realm gave and no address in it.
func TestAnInvitationTheRealmCannotMailLeavesNoLink(t *testing.T) {
	h, k := startAgainstARealmThatCannotMail(t)
	if status, body := h.do(t, http.MethodPost, invitationPath("demo"), h.token(t, "gentian", "alice"),
		`{"recoveryEmail":"`+owner+`"}`); status != http.StatusAccepted {
		t.Fatalf("request: %d %v", status, body)
	}
	h.registrar.SendInvitations(context.Background())

	var putAccount, mail bool
	for _, w := range k.wrote() {
		switch {
		case strings.Contains(w, "/gentian-activation/"):
			t.Fatalf("a link was issued with nobody to show it to: %v", k.wrote())
		case strings.HasSuffix(w, "/execute-actions-email"):
			mail = true
		case w == "PUT /admin/realms/demo/users/root1":
			putAccount = true
		}
	}
	if !putAccount || !mail {
		t.Fatalf("the address is put on the account and the mail is tried: %v", k.wrote())
	}
	if _, waiting := h.invitations.request("demo"); waiting {
		t.Fatal("the request is kept though it cannot be mailed")
	}
	o := h.invitations.outcome(t, "demo")
	if o.Action != record.ActionInvitationNotMailed || o.Target != "admin@demo."+dt.KernelDomain {
		t.Fatalf("recorded as %+v", o)
	}
	if !strings.Contains(o.Detail, "HTTP 500: Failed to send execute actions email: 535 Authentication failed") {
		t.Fatalf("the reason the realm gave is not recorded: %q", o.Detail)
	}
	state := h.invitationState(t, "demo")
	if state["state"] != "not-mailed" || state["reason"] != o.Detail {
		t.Fatalf("state: %v", state)
	}
	raw, _ := json.Marshal(state)
	nothingNames(t, "the state", string(raw), owner, shownLink, "one-time-key")
	nothingNames(t, "the record", o.Detail, owner, shownLink, "one-time-key")
	nothingNames(t, "the log", h.logged.String(), owner, shownLink, "one-time-key")

	// By hand, as before: the route issues the link for the caller to show.
	status, body := h.do(t, http.MethodPost, "/v1/clusters/"+dt.Cluster+"/tenants/demo/actions/activate-admin",
		h.token(t, "gentian", "alice"), `{"recoveryEmail":"`+owner+`"}`)
	activation, _ := body["activation"].(map[string]any)
	if status != http.StatusOK || activation["link"] != shownLink {
		t.Fatalf("activating by hand afterwards: %d %v", status, body)
	}
	if reason, _ := activation["mailError"].(string); !strings.Contains(reason, "535 Authentication failed") || strings.Contains(reason, owner) {
		t.Fatalf("the route passes the realm's reason on, without the address: %q", reason)
	}
}

// An account the registrar does not hand over to anybody -- one that holds a
// platform role -- is not handed over this way either, and is not asked for
// again every round until the request expires.
func TestAnInvitationForAProtectedAccountIsRefusedOnce(t *testing.T) {
	h, k := startAgainstARealm(t, "admin")
	if status, body := h.do(t, http.MethodPost, invitationPath("demo"), h.token(t, "gentian", "alice"),
		`{"recoveryEmail":"`+owner+`"}`); status != http.StatusAccepted {
		t.Fatalf("request: %d %v", status, body)
	}
	h.registrar.SendInvitations(context.Background())
	if w := k.wrote(); len(w) != 0 {
		t.Fatalf("refused, and still wrote %v", w)
	}
	if _, waiting := h.invitations.request("demo"); waiting {
		t.Fatal("a refused request is kept")
	}
	if o := h.invitations.outcome(t, "demo"); o.Action != record.ActionInvitationRefused {
		t.Fatalf("recorded as %+v", o)
	}
	nothingNames(t, "the log", h.logged.String(), owner)
}

// Whoever may not bring tenants on may not ask for an administrator's
// invitation, nor learn that one is waiting -- a tenant's own administrator
// included, for their own tenant. Nothing is kept for them.
func TestOnlyWhoMayConfigureTheClusterAsksForAnInvitation(t *testing.T) {
	h, _ := startArriving(t)
	c := "/v1/clusters/" + dt.Cluster
	for name, token := range map[string]string{
		"a tenant administrator": h.token(t, "tenant-demo", "tom"),
		"a member":               h.token(t, "tenant-demo", "mia"),
	} {
		for _, rt := range []struct{ method, path, body string }{
			{http.MethodPost, invitationPath("newco"), `{"recoveryEmail":"` + owner + `"}`},
			{http.MethodPost, invitationPath("demo"), `{"recoveryEmail":"` + owner + `"}`},
			{http.MethodGet, c + "/tenants/demo/admin-invitation", ""},
			{http.MethodGet, c + "/admin-invitations", ""},
		} {
			if status, _ := h.do(t, rt.method, rt.path, token, rt.body); status != http.StatusForbidden {
				t.Errorf("%s: %s %s answered %d, want 403", name, rt.method, rt.path, status)
			}
		}
	}
	if status, _ := h.do(t, http.MethodPost, invitationPath("newco"), "", `{"recoveryEmail":"`+owner+`"}`); status != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", status)
	}
	if waiting, _ := h.invitations.PendingInvitations(context.Background()); len(waiting) != 0 {
		t.Fatalf("a refused request was kept: %+v", waiting)
	}
}

// Only with an address: without one the link has to be shown to somebody,
// and that is the other route's. And only what is a tenant name and an
// address is kept.
func TestARequestWithoutAnAddressIsNotKept(t *testing.T) {
	h, _ := startArriving(t)
	alice := h.token(t, "gentian", "alice")
	for what, c := range map[string]struct{ tenant, body string }{
		"no address":        {"newco", `{}`},
		"an empty address":  {"newco", `{"recoveryEmail":"  "}`},
		"not an address":    {"newco", `{"recoveryEmail":"owner"}`},
		"no body":           {"newco", ``},
		"not a tenant name": {"New_Co", `{"recoveryEmail":"` + owner + `"}`},
		"an unknown field":  {"newco", `{"recoveryEmail":"` + owner + `","link":true}`},
	} {
		status, _ := h.do(t, http.MethodPost, invitationPath(c.tenant), alice, c.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: answered %d, want 400", what, status)
		}
	}
	if waiting, _ := h.invitations.PendingInvitations(context.Background()); len(waiting) != 0 {
		t.Fatalf("kept: %+v", waiting)
	}
}
