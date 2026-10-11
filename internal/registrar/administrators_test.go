/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/registrar"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// The registrar does not change who holds a platform role, whoever asks.
//
// These run the real Keycloak client behind the real routes, against a realm
// that answers like Keycloak, so what is shown is the whole path: a caller
// the store ALLOWS asks for each thing the rule covers, is answered 403, and
// nothing is written to the realm. And they run once for every role the
// Cluster claim's schema can name, with the group read off a claim the way
// the registrar reads it: the rule is the same for all of them.
//
// The group here is named in the tenant's own subtree rather than
// gentian:platform:admin. The routes compose a new group's name under the
// tenant's prefix and refuse the label "admin" before anything else looks at
// it, so with the default name "create a group of that name" could not be
// asked through a route at all; with this one every refusal below is the
// rule's own. It also shows the name is whatever the Cluster claim says, not
// a constant. The default name is covered where the rule lives
// (identity/guard_test.go).
const chiefs = "gentian:tenant:demo:chiefs"

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
	custom := map[string][]string{identity.CustomGroupAttribute: {"true"}}
	groups := []map[string]any{
		{"id": "g-chiefs", "name": chiefs, "path": "/" + chiefs, "attributes": custom},
		{"id": "g-sales", "name": "gentian:tenant:demo:sales", "path": "/gentian:tenant:demo:sales", "attributes": custom},
	}
	users := map[string]map[string]any{
		"root1": {"id": "root1", "username": "admin@demo." + dt.KernelDomain, "enabled": true, "emailVerified": true},
		"u1":    {"id": "u1", "username": "ada@demo." + dt.KernelDomain, "enabled": true, "emailVerified": true},
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/realms/demo")
	answer := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case path == "/groups":
		answer(groups)
	case strings.HasPrefix(path, "/groups/"):
		for _, g := range groups {
			if path == "/groups/"+g["id"].(string) {
				answer(g)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case path == "/users":
		out := []any{}
		for _, u := range users {
			if u["username"] == r.URL.Query().Get("username") {
				out = append(out, u)
			}
		}
		answer(out)
	case strings.HasSuffix(path, "/groups") && strings.HasPrefix(path, "/users/"):
		// root1 holds the platform role under test; u1 is in sales.
		if strings.HasPrefix(path, "/users/root1/") {
			answer(groups[:1])
			return
		}
		answer(groups[1:])
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

// startAgainstARealm runs the routes against a realm whose group chiefs holds
// the platform role the Cluster claim names it for under role, a field of
// spec.platformRoles.
func startAgainstARealm(t *testing.T, role string) (*harness, *realm) {
	t.Helper()
	k := &realm{}
	kc := httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(kc.Close)
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		claim("main", layout.Namespace(layout.Provisioning),
			map[string]any{"platformRoles": map[string]any{role: chiefs}})).Build()
	client, err := identity.New(identity.Config{
		BaseURL: kc.URL,
		Source:  identity.StaticSource{"demo": {Realm: "demo", ClientID: identity.ClientID, ClientSecret: "s"}},
		// What the registrar reads off the Cluster claim, read the way it
		// reads it.
		PlatformRoleGroups: registrar.ClaimPlatformRoleGroups(reader),
	})
	if err != nil {
		t.Fatal(err)
	}
	return startWithIdentity(t, client), k
}

func TestTheRoutesRefuseToChangeWhoHoldsAPlatformRole(t *testing.T) {
	for _, role := range platformRoleFields(t) {
		t.Run(role, func(t *testing.T) { theRoutesRefuse(t, role) })
	}
}

func theRoutesRefuse(t *testing.T, role string) {
	activate := "/v1/clusters/" + dt.Cluster + "/tenants/demo/actions/activate-admin"
	for _, c := range []struct {
		what, path, body string
	}{
		{"adding somebody to the group", "set-membership",
			`{"person":"u1","group":"` + chiefs + `","member":true}`},
		{"removing somebody from the group", "set-membership",
			`{"person":"root1","group":"` + chiefs + `","member":false}`},
		{"inviting somebody into the group", "invite-person",
			`{"email":"eve@example.com","groups":["` + chiefs + `"]}`},
		{"renaming the group", "rename-group", `{"group":"` + chiefs + `","name":"former"}`},
		{"deleting the group", "delete-group", `{"group":"` + chiefs + `"}`},
		{"creating a group of that name", "create-group", `{"name":"chiefs"}`},
		{"renaming another group to that name", "rename-group",
			`{"group":"gentian:tenant:demo:sales","name":"chiefs"}`},
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
		if msg, _ := body["error"].(string); !strings.Contains(msg, "holds a platform role") || !strings.Contains(msg, chiefs) {
			t.Errorf("%s: the refusal should say why and name the group: %v", c.what, body)
		}
		if w := k.wrote(); len(w) != 0 {
			t.Errorf("%s: refused, and still wrote %v", c.what, w)
		}
	}
}

// The rule is about the holders of platform roles and nobody else: the same
// acts on another person, and another group, go through.
func TestEverybodyElseIsManagedAsBefore(t *testing.T) {
	for _, c := range []struct {
		what, path, body string
	}{
		{"changing somebody's address", "update-person", `{"person":"u1","email":"ada@example.org"}`},
		{"removing somebody", "remove-person", `{"person":"u1"}`},
		{"removing somebody's second factor", "remove-totp", `{"person":"u1"}`},
		{"mailing somebody a password link", "send-password-reset", `{"person":"u1"}`},
		{"putting somebody in another group", "set-membership",
			`{"person":"u1","group":"gentian:tenant:demo:sales","member":true}`},
		{"putting a role holder in another group", "set-membership",
			`{"person":"root1","group":"gentian:tenant:demo:sales","member":true}`},
		{"inviting somebody", "invite-person",
			`{"email":"eve@example.com","groups":["gentian:tenant:demo:sales"]}`},
		{"deleting another group", "delete-group", `{"group":"gentian:tenant:demo:sales"}`},
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
