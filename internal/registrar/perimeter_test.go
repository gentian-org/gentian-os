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

// Who approves a tenant's public addresses is changed only by somebody who
// may approve them.
//
// The members of gentian:tenant:<t>:perimeter hold tenant#can_expose. A
// tenant's administrator manages the tenant's people and, unless the
// cluster's administrator said otherwise on the tenant's manifest, does not
// hold it. These run the real Keycloak client behind the real routes against
// a realm that answers like Keycloak, for the two cases the model's fixture
// has: tina administers solo and may not approve there, tom administers demo
// where the administrators approve. The same requests, refused for one with
// nothing written, carried out for the other.

// approversRealm is enough of Keycloak for one tenant: the perimeter group
// with p1 in it, a custom group with u1 in it.
type approversRealm struct {
	tenant string
	mu     sync.Mutex
	writes []string
}

func (k *approversRealm) wrote() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.writes...)
}

func (k *approversRealm) serve(w http.ResponseWriter, r *http.Request) {
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
			w.Header().Set("Location", "https://kc/admin/realms/"+k.tenant+"/users/new1")
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	prefix := "gentian:tenant:" + k.tenant + ":"
	groups := []map[string]any{
		// As the tenant's composition makes it: no custom mark.
		{"id": "g-perimeter", "name": prefix + "perimeter", "path": "/" + prefix + "perimeter"},
		{"id": "g-sales", "name": prefix + "sales", "path": "/" + prefix + "sales",
			"attributes": map[string][]string{identity.CustomGroupAttribute: {"true"}}},
	}
	domain := "@" + k.tenant + "." + dt.KernelDomain
	users := map[string]map[string]any{
		"p1": {"id": "p1", "username": "pat" + domain, "enabled": true, "emailVerified": true},
		"u1": {"id": "u1", "username": "ada" + domain, "enabled": true, "emailVerified": true},
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/realms/"+k.tenant)
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
		answer([]any{})
	case strings.HasSuffix(path, "/groups") && strings.HasPrefix(path, "/users/"):
		if strings.HasPrefix(path, "/users/p1/") {
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

func startAgainstApprovers(t *testing.T, tenant string) (*harness, *approversRealm) {
	t.Helper()
	k := &approversRealm{tenant: tenant}
	kc := httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(kc.Close)
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		claim("main", layout.Namespace(layout.Provisioning),
			map[string]any{"platformRoles": map[string]any{"admin": platformAdmins}})).Build()
	client, err := identity.New(identity.Config{
		BaseURL:            kc.URL,
		Source:             identity.StaticSource{tenant: {Realm: tenant, ClientID: identity.ClientID, ClientSecret: "s"}},
		PlatformRoleGroups: registrar.ClaimPlatformRoleGroups(reader),
	})
	if err != nil {
		t.Fatal(err)
	}
	return startWithIdentity(t, client), k
}

// approverWrites is everything that would change who approves: the group's
// membership, the group, and an account in it.
func approverWrites(tenant string) []struct{ what, path, body string } {
	group := "gentian:tenant:" + tenant + ":perimeter"
	return []struct{ what, path, body string }{
		{"adding somebody to the group", "set-membership", `{"person":"u1","group":"` + group + `","member":true}`},
		{"removing somebody from the group", "set-membership", `{"person":"p1","group":"` + group + `","member":false}`},
		{"inviting somebody into the group", "invite-person", `{"email":"eve@example.com","groups":["` + group + `"]}`},
		{"changing an approver's address", "update-person", `{"person":"p1","email":"eve@example.com"}`},
		{"disabling an approver", "update-person", `{"person":"p1","enabled":false}`},
		{"removing an approver", "remove-person", `{"person":"p1"}`},
		{"removing an approver's second factor", "remove-totp", `{"person":"p1"}`},
		{"mailing an approver a password link", "send-password-reset", `{"person":"p1"}`},
	}
}

func TestATenantAdministratorWhoMayNotApproveDoesNotSayWhoDoes(t *testing.T) {
	group := "gentian:tenant:solo:perimeter"
	writes := append(approverWrites("solo"), []struct{ what, path, body string }{
		{"renaming the group", "rename-group", `{"group":"` + group + `","name":"former"}`},
		{"deleting the group", "delete-group", `{"group":"` + group + `"}`},
	}...)
	for _, c := range writes {
		h, k := startAgainstApprovers(t, "solo")
		status, body := h.do(t, http.MethodPost, "/v1/tenants/solo/actions/"+c.path, h.token(t, "tenant-solo", "tina"), c.body)
		if status != http.StatusForbidden {
			t.Errorf("%s: answered %d %v, want 403", c.what, status, body)
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, "who approves this tenant's public addresses") || !strings.Contains(msg, group) {
			t.Errorf("%s: the refusal should say why and name the group: %v", c.what, body)
		}
		if w := k.wrote(); len(w) != 0 {
			t.Errorf("%s: refused, and still wrote %v", c.what, w)
		}
	}

	// The name is the platform's, for a group that is not there yet as much
	// as for one that is: nobody makes a group of that name, or renames one
	// to it, and so nobody holds a perimeter group they made themselves.
	for what, call := range map[string][2]string{
		"creating a group of that name":       {"create-group", `{"name":"perimeter"}`},
		"renaming another group to that name": {"rename-group", `{"group":"gentian:tenant:solo:sales","name":"perimeter"}`},
	} {
		h, k := startAgainstApprovers(t, "solo")
		status, body := h.do(t, http.MethodPost, "/v1/tenants/solo/actions/"+call[0], h.token(t, "tenant-solo", "tina"), call[1])
		if status != http.StatusBadRequest || len(k.wrote()) != 0 {
			t.Errorf("%s: answered %d %v and wrote %v, want 400 and nothing", what, status, body, k.wrote())
		}
	}

	// Everybody and everything else in the tenant is hers to manage as before.
	for _, c := range []struct{ what, path, body string }{
		{"changing somebody's address", "update-person", `{"person":"u1","email":"ada@example.org"}`},
		{"putting somebody in another group", "set-membership", `{"person":"u1","group":"gentian:tenant:solo:sales","member":true}`},
		{"putting an approver in another group", "set-membership", `{"person":"p1","group":"gentian:tenant:solo:sales","member":true}`},
		{"inviting somebody", "invite-person", `{"email":"eve@example.com","groups":["gentian:tenant:solo:sales"]}`},
	} {
		h, k := startAgainstApprovers(t, "solo")
		status, body := h.do(t, http.MethodPost, "/v1/tenants/solo/actions/"+c.path, h.token(t, "tenant-solo", "tina"), c.body)
		if status >= 300 || len(k.wrote()) == 0 {
			t.Errorf("%s: answered %d %v and wrote %v", c.what, status, body, k.wrote())
		}
	}
}

// Whoever may approve says who else does: a tenant's administrator where the
// administrators approve, and the cluster's administrator in a tenant its
// cluster operates.
func TestWhoeverMayApproveSaysWhoElseDoes(t *testing.T) {
	for _, who := range [][2]string{{"tenant-demo", "tom"}, {"gentian", "alice"}} {
		for _, c := range approverWrites("demo") {
			h, k := startAgainstApprovers(t, "demo")
			status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/"+c.path, h.token(t, who[0], who[1]), c.body)
			if status >= 300 || len(k.wrote()) == 0 {
				t.Errorf("%s, by %s: answered %d %v and wrote %v", c.what, who[1], status, body, k.wrote())
			}
		}
		// The group itself is the platform's, for them too: it is made with
		// the tenant and stays.
		h, k := startAgainstApprovers(t, "demo")
		status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/delete-group", h.token(t, who[0], who[1]),
			`{"group":"gentian:tenant:demo:perimeter"}`)
		if status != http.StatusBadRequest || len(k.wrote()) != 0 {
			t.Errorf("deleting the group, by %s: answered %d and wrote %v, want 400 and nothing", who[1], status, k.wrote())
		}
	}
}

// A store that does not answer the second question holds back: the write is
// refused where it would change who approves.
func TestAnUnansweredQuestionHoldsBack(t *testing.T) {
	k := &approversRealm{tenant: "demo"}
	kc := httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(kc.Close)
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		claim("main", layout.Namespace(layout.Provisioning),
			map[string]any{"platformRoles": map[string]any{"admin": platformAdmins}})).Build()
	client, err := identity.New(identity.Config{
		BaseURL:            kc.URL,
		Source:             identity.StaticSource{"demo": {Realm: "demo", ClientID: identity.ClientID, ClientSecret: "s"}},
		PlatformRoleGroups: registrar.ClaimPlatformRoleGroups(reader),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := startWith(t, client, unsureAboutApproval{facts})
	status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/set-membership", h.token(t, "tenant-demo", "tom"),
		`{"person":"u1","group":"gentian:tenant:demo:perimeter","member":true}`)
	if status != http.StatusForbidden || len(k.wrote()) != 0 {
		t.Errorf("answered %d %v and wrote %v, want 403 and nothing", status, body, k.wrote())
	}
}

// unsureAboutApproval answers every question from the table but the one
// about approving, which it cannot answer.
type unsureAboutApproval struct{ dt.Table }

func (u unsureAboutApproval) Check(ctx context.Context, id, user, relation, object string) (bool, error) {
	if relation == "can_expose" {
		return false, errors.New("connection refused")
	}
	return u.Table.Check(ctx, id, user, relation, object)
}
