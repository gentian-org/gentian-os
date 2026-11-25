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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/registrar"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// The registrar does not change the account of who holds a cluster role,
// whoever asks.
//
// These run the real Keycloak client behind the real routes, against a realm
// that answers like Keycloak, so what is shown is the whole path: a caller
// the store ALLOWS asks for each thing the rule covers, is answered 403, and
// nothing is written to the realm. And they run once for each of several
// roles, with the holder read off a claim the way the registrar reads it:
// the rule is the same for all of them, the cluster's administrator
// included.

// realm is enough of Keycloak for the rule to be asked and for a refused
// write to be noticed.
type realm struct {
	mu     sync.Mutex
	writes []string
}

func (k *realm) wrote() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.writes...)
}

func (k *realm) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
		_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
		return
	}
	if r.Method != http.MethodGet {
		k.mu.Lock()
		k.writes = append(k.writes, r.Method+" "+r.URL.Path)
		k.mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users") {
			w.Header().Set("Location", "https://kc/admin/realms/demo/users/new1")
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	users := map[string]map[string]any{
		"root1": {"id": "root1", "username": "admin@demo." + dt.KernelDomain, "enabled": true, "emailVerified": true},
		"u1":    {"id": "u1", "username": "ada@demo." + dt.KernelDomain, "enabled": true, "emailVerified": true},
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/realms/demo")
	answer := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case path == "/users":
		out := []any{}
		for _, u := range users {
			if u["username"] == r.URL.Query().Get("username") {
				out = append(out, u)
			}
		}
		answer(out)
	case strings.HasSuffix(path, "/credentials"):
		answer([]map[string]string{{"id": "otp1", "type": "otp"}})
	case strings.HasPrefix(path, "/users/"):
		if u, ok := users[strings.TrimPrefix(path, "/users/")]; ok {
			answer(u)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		answer([]any{})
	}
}

// startAgainstARealm runs the routes against a realm whose account root1 the
// Cluster claim names a holder of role, as a person of the platform tenant.
func startAgainstARealm(t *testing.T, role string) (*harness, *realm) {
	t.Helper()
	k := &realm{}
	kc := httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(kc.Close)
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		claim("main", layout.Namespace(layout.Provisioning),
			map[string]any{"platformAccess": map[string]any{"roleHolders": []any{
				map[string]any{"role": role, "tenant": "platform", "subject": "root1", "username": "administrator"},
			}}})).Build()
	client, err := identity.New(identity.Config{
		BaseURL: kc.URL,
		Source:  identity.StaticSource{"demo": {Realm: "demo", ClientID: identity.ClientID, ClientSecret: "s"}},
		// What the registrar reads off the Cluster claim, read the way it
		// reads it.
		PlatformRoleHolders: registrar.ClaimPlatformRoleHolders(reader),
	})
	if err != nil {
		t.Fatal(err)
	}
	return startWithIdentity(t, client), k
}

func TestTheRoutesRefuseToChangeWhoHoldsAClusterRole(t *testing.T) {
	for _, role := range []string{"admin", "breakGlass", "securityOfficer", "auditor"} {
		t.Run(role, func(t *testing.T) { theRoutesRefuse(t, role) })
	}
}

func theRoutesRefuse(t *testing.T, role string) {
	activate := "/v1/clusters/" + dt.Cluster + "/tenants/demo/actions/activate-admin"
	for _, c := range []struct {
		what, path, body string
	}{
		{"changing a role holder's address", "update-person",
			`{"person":"root1","email":"eve@example.com"}`},
		{"disabling a role holder", "update-person", `{"person":"root1","enabled":false}`},
		{"removing a role holder", "remove-person", `{"person":"root1"}`},
		{"requiring a second factor of a role holder", "require-totp", `{"person":"root1","mail":true}`},
		{"removing a role holder's second factor", "remove-totp", `{"person":"root1"}`},
		{"mailing a role holder a password link", "send-password-reset", `{"person":"root1"}`},
		{"issuing an activation link for a role holder's account", activate,
			`{"recoveryEmail":"eve@example.com"}`},
	} {
		h, k := startAgainstARealm(t, role)
		// tom may manage demo's people and alice may configure the cluster:
		// each is allowed the route, and refused what it would do.
		token, path := h.token(t, "tenant-demo", "tom"), "/v1/tenants/demo/actions/"+c.path
		if c.path == activate {
			token, path = h.token(t, "gentian", "alice"), activate
		}
		status, body := h.do(t, http.MethodPost, path, token, c.body)
		if status != http.StatusForbidden {
			t.Errorf("%s: answered %d %v, want 403", c.what, status, body)
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, "holds a cluster role") {
			t.Errorf("%s: the refusal should say why: %v", c.what, body)
		}
		if w := k.wrote(); len(w) != 0 {
			t.Errorf("%s: refused, and still wrote %v", c.what, w)
		}
	}
}

// The rule is about the holders of cluster roles and nobody else: the same
// acts on another person go through.
func TestEverybodyElseIsManagedAsBefore(t *testing.T) {
	for _, c := range []struct {
		what, path, body string
	}{
		{"changing somebody's address", "update-person", `{"person":"u1","email":"ada@example.org"}`},
		{"removing somebody", "remove-person", `{"person":"u1"}`},
		{"removing somebody's second factor", "remove-totp", `{"person":"u1"}`},
		{"mailing somebody a password link", "send-password-reset", `{"person":"u1"}`},
		{"inviting somebody", "invite-person", `{"email":"eve@example.com"}`},
	} {
		h, k := startAgainstARealm(t, "admin")
		status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/"+c.path,
			h.token(t, "tenant-demo", "tom"), c.body)
		if status >= 300 {
			t.Errorf("%s: answered %d %v", c.what, status, body)
		}
		if len(k.wrote()) == 0 {
			t.Errorf("%s: nothing was written", c.what)
		}
	}
}

// The group routes are gone: what a person holds is a position or a role in
// the rights store, and an invitation that names groups is refused whole.
func TestNothingIsDoneWithGroups(t *testing.T) {
	h, k := startAgainstARealm(t, "admin")
	token := h.token(t, "tenant-demo", "tom")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/tenants/demo/groups", ""},
		{http.MethodGet, "/v1/tenants/demo/group-members?group=x", ""},
		{http.MethodPost, "/v1/tenants/demo/actions/set-membership", `{"person":"u1","group":"x","member":true}`},
		{http.MethodPost, "/v1/tenants/demo/actions/create-group", `{"name":"sales"}`},
		{http.MethodPost, "/v1/tenants/demo/actions/delete-group", `{"group":"x"}`},
		{http.MethodPost, "/v1/tenants/demo/actions/rename-group", `{"group":"x","name":"y"}`},
	} {
		if status, body := h.do(t, c.method, c.path, token, c.body); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: answered %d %v, want no such route", c.method, c.path, status, body)
		}
	}
	if status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/invite-person", token,
		`{"email":"eve@example.com","groups":["gentian:tenant:demo:sales"]}`); status != http.StatusBadRequest {
		t.Errorf("an invitation into a group: answered %d %v, want 400", status, body)
	}
	if w := k.wrote(); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}

// A tenant that shares a realm manages its own people and nobody else's:
// who they are is the rights store's answer, the holders of its position
// user. The realm's other accounts are not found, and are not written to.
func TestATenantThatSharesARealmManagesItsOwnPeopleAlone(t *testing.T) {
	k := &realm{}
	kc := httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(kc.Close)
	client, err := identity.New(identity.Config{
		BaseURL:             kc.URL,
		Source:              identity.StaticSource{"demo": {Realm: "demo", ClientID: identity.ClientID, ClientSecret: "s"}},
		PlatformRoleHolders: func(context.Context) ([]string, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := registrar.Tenant{Name: "solo", Realm: "demo", LoginDomain: "solo." + dt.KernelDomain}
	h := startWithTenants(t, client, nil, fakeTenants{"demo": ownRealm("demo"), "solo": shared}, newFakeMailboxes())
	if _, err := h.rights.Give(context.Background(), "test", "solo", "u1", authz.Assignment{Position: authz.PositionUser}); err != nil {
		t.Fatal(err)
	}
	tina := h.token(t, "tenant-solo", "tina")

	if status, body := h.do(t, http.MethodGet, "/v1/tenants/solo/people/u1", tina, ""); status != http.StatusOK {
		t.Errorf("the tenant's own person: %d %v", status, body)
	}
	if status, body := h.do(t, http.MethodGet, "/v1/tenants/solo/people/root1", tina, ""); status != http.StatusNotFound {
		t.Errorf("another tenant's person in the same realm: %d %v, want 404", status, body)
	}
	for _, act := range []string{"update-person", "remove-person", "remove-totp", "send-password-reset"} {
		status, body := h.do(t, http.MethodPost, "/v1/tenants/solo/actions/"+act, tina, `{"person":"root1","enabled":false}`)
		if status != http.StatusBadRequest && status != http.StatusNotFound {
			t.Errorf("%s on another tenant's person: %d %v, want it refused as not this tenant's", act, status, body)
		}
	}
	if w := k.wrote(); len(w) != 0 {
		t.Errorf("another tenant's person was written to: %v", w)
	}
	// The tenant with the realm to itself still sees everybody in it.
	if status, body := h.do(t, http.MethodGet, "/v1/tenants/demo/people/root1", h.token(t, "tenant-demo", "tom"), ""); status != http.StatusOK {
		t.Errorf("the realm's own tenant: %d %v", status, body)
	}

	// An invitation makes the account, mails it, and gives it the position
	// that makes it the tenant's.
	status, body := h.do(t, http.MethodPost, "/v1/tenants/solo/actions/invite-person", tina, `{"email":"eve@example.com"}`)
	if status != http.StatusAccepted || body["mailed"] != true {
		t.Fatalf("an invitation in a shared realm: %d %v", status, body)
	}
	if got := h.rights.positionsOf("solo", "new1"); len(got) != 1 || got[0] != authz.PositionUser {
		t.Errorf("the invited person holds %v in the tenant, want user", got)
	}
}
