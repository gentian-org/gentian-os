/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRealm is enough of one Keycloak realm to provision a client in: the
// clients it has, the service account user, and which roles that user holds.
type fakeRealm struct {
	mu sync.Mutex

	// registrar is nil until it is created.
	registrar *keycloakClientRecord
	// granted is the set of realm-management roles the service account holds.
	granted map[string]bool
	// scoped is the set the client's scope maps, i.e. what its tokens carry.
	scoped map[string]bool
	// calls records method+path, for asserting what was and was not done.
	calls []string
	// createdBody is the representation the client was created with.
	createdBody map[string]any
	// updatedBody is the representation it was re-asserted with, if any.
	updatedBody map[string]any
	// noManagement makes the realm answer with no realm-management client.
	noManagement bool
	// retired says the realm still has the client the director used to hold.
	retired bool
}

func newFakeRealm() *fakeRealm {
	return &fakeRealm{granted: map[string]bool{}, scoped: map[string]bool{}}
}

// allRealmManagementRoles is what a real realm offers; the registrar must take
// only its five from it.
var allRealmManagementRoles = []string{
	"view-users", "query-users", "query-groups", "manage-users", "manage-realm",
	"manage-clients", "manage-identity-providers", "impersonation", "view-events",
	"realm-admin",
}

func (f *fakeRealm) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))

		case r.URL.Path == "/admin/realms/demo/clients" && r.Method == http.MethodGet:
			switch r.URL.Query().Get("clientId") {
			case RegistrarClientID:
				f.mu.Lock()
				d := f.registrar
				f.mu.Unlock()
				if d == nil {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				_ = json.NewEncoder(w).Encode([]keycloakClientRecord{*d})
			case "realm-management":
				if f.noManagement {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				_ = json.NewEncoder(w).Encode([]keycloakClientRecord{
					{ID: "mgmt-uuid", ClientID: "realm-management"}})
			case RetiredDirectorClientID:
				f.mu.Lock()
				still := f.retired
				f.mu.Unlock()
				if !still {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				_ = json.NewEncoder(w).Encode([]keycloakClientRecord{
					{ID: "director-uuid", ClientID: RetiredDirectorClientID, Enabled: true}})
			default:
				_, _ = w.Write([]byte(`[]`))
			}

		case r.URL.Path == "/admin/realms/demo/clients" && r.Method == http.MethodPost:
			f.mu.Lock()
			_ = json.Unmarshal(body, &f.createdBody)
			f.registrar = &keycloakClientRecord{
				ID: "registrar-uuid", ClientID: RegistrarClientID, Enabled: true,
				PublicClient: false, ServiceAccountsEnabled: true, StandardFlowEnabled: false,
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)

		case r.URL.Path == "/admin/realms/demo/clients/registrar-uuid" && r.Method == http.MethodPut:
			f.mu.Lock()
			_ = json.Unmarshal(body, &f.updatedBody)
			if f.registrar != nil {
				f.registrar.Enabled = true
				f.registrar.PublicClient = false
				f.registrar.ServiceAccountsEnabled = true
				f.registrar.StandardFlowEnabled = false
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.URL.Path == "/admin/realms/demo/clients/registrar-uuid/service-account-user":
			_, _ = w.Write([]byte(`{"id":"sa-uuid","username":"service-account-gentian-registrar-admin"}`))

		case r.URL.Path == "/admin/realms/demo/users/sa-uuid/role-mappings/clients/mgmt-uuid/available":
			f.mu.Lock()
			var out []keycloakRoleRecord
			for _, name := range allRealmManagementRoles {
				if !f.granted[name] {
					out = append(out, keycloakRoleRecord{ID: "role-" + name, Name: name})
				}
			}
			f.mu.Unlock()
			if out == nil {
				out = []keycloakRoleRecord{}
			}
			_ = json.NewEncoder(w).Encode(out)

		case r.URL.Path == "/admin/realms/demo/users/sa-uuid/role-mappings/clients/mgmt-uuid" && r.Method == http.MethodPost:
			var roles []keycloakRoleRecord
			_ = json.Unmarshal(body, &roles)
			f.mu.Lock()
			for _, r := range roles {
				f.granted[r.Name] = true
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.URL.Path == "/admin/realms/demo/clients/registrar-uuid/scope-mappings/clients/mgmt-uuid/available":
			f.mu.Lock()
			out := []keycloakRoleRecord{}
			for _, name := range allRealmManagementRoles {
				if !f.scoped[name] {
					out = append(out, keycloakRoleRecord{ID: "role-" + name, Name: name})
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)

		case r.URL.Path == "/admin/realms/demo/clients/registrar-uuid/scope-mappings/clients/mgmt-uuid" && r.Method == http.MethodPost:
			var roles []keycloakRoleRecord
			_ = json.Unmarshal(body, &roles)
			f.mu.Lock()
			for _, r := range roles {
				f.scoped[r.Name] = true
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.URL.Path == "/admin/realms/demo/clients/registrar-uuid/client-secret":
			_, _ = w.Write([]byte(`{"type":"secret","value":"s3cr3t"}`))

		case r.URL.Path == "/admin/realms/demo/clients/registrar-uuid" && r.Method == http.MethodDelete:
			f.mu.Lock()
			f.registrar = nil
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.URL.Path == "/admin/realms/demo/clients/director-uuid" && r.Method == http.MethodDelete:
			f.mu.Lock()
			f.retired = false
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeRealm) did(method, path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == method+" "+path {
			return true
		}
	}
	return false
}

func (f *fakeRealm) heldRoles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name, ok := range f.granted {
		if ok {
			out = append(out, name)
		}
	}
	return out
}

func TestEnsureRegistrarRealmClient_CreatesAConfidentialServiceAccount(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	secret, err := c.EnsureRegistrarRealmClient(context.Background(), "demo")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if secret != "s3cr3t" {
		t.Errorf("secret = %q", secret)
	}

	f.mu.Lock()
	created := f.createdBody
	f.mu.Unlock()
	if created == nil {
		t.Fatal("no client was created")
	}
	// Nobody signs in as this. A credential that can be used interactively is
	// one that can be phished, and a public client is not a credential at all.
	for field, want := range map[string]any{
		"publicClient":              false,
		"serviceAccountsEnabled":    true,
		"standardFlowEnabled":       false,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"fullScopeAllowed":          false,
	} {
		if created[field] != want {
			t.Errorf("%s = %v, want %v", field, created[field], want)
		}
	}
}

// The list of roles IS the security statement: everything the registrar can do
// in a realm is on it, so anything else turning up is a change nobody made
// deliberately.
func TestEnsureRegistrarRealmClient_TakesOnlyItsOwnRoles(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	if _, err := c.EnsureRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, r := range f.heldRoles() {
		held[r] = true
	}
	for _, want := range registrarRealmRoles {
		if !held[want] {
			t.Errorf("role %q was not granted", want)
		}
		delete(held, want)
	}
	for extra := range held {
		t.Errorf("role %q was granted and is not on the list", extra)
	}
	// realm-admin is the one that would make the whole design pointless.
	for _, r := range f.heldRoles() {
		if r == "realm-admin" || r == "impersonation" {
			t.Fatalf("the registrar holds %q", r)
		}
	}
}

// A second pass must not re-create, re-grant or rotate anything. This is what
// a reconciler does on every loop.
func TestEnsureRegistrarRealmClient_IsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	if _, err := c.EnsureRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	secret, err := c.EnsureRegistrarRealmClient(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if secret != "s3cr3t" {
		t.Errorf("secret changed on a reconcile: %q", secret)
	}
	if f.did(http.MethodPost, "/admin/realms/demo/clients") {
		t.Error("the client was created a second time")
	}
	if f.did(http.MethodPost, "/admin/realms/demo/users/sa-uuid/role-mappings/clients/mgmt-uuid") {
		t.Error("roles were granted again when they were already held")
	}
	if f.did(http.MethodPut, "/admin/realms/demo/clients/registrar-uuid") {
		t.Error("a client already in the right shape was rewritten")
	}
	if f.did(http.MethodPost, "/admin/realms/demo/clients/registrar-uuid/scope-mappings/clients/mgmt-uuid") {
		t.Error("the scope was mapped again when it already carried the roles")
	}
}

// Holding a role is not carrying it. The client is fullScopeAllowed: false, so
// its tokens carry only what its scope maps; without the mapping the service
// account held all five roles and Keycloak answered every People call 403.
// The scope must carry exactly the granted five -- realm-admin in it would
// undo the point of the narrow scope.
func TestEnsureRegistrarRealmClient_TokensCarryTheGrantedRoles(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	if _, err := c.EnsureRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, want := range registrarRealmRoles {
		if !f.scoped[want] {
			t.Errorf("role %q is held but not in the client's scope: its tokens will not carry it", want)
		}
	}
	for name := range f.scoped {
		wanted := false
		for _, w := range registrarRealmRoles {
			wanted = wanted || w == name
		}
		if !wanted {
			t.Errorf("role %q is in the client's scope and is not on the list", name)
		}
	}
}

// A client somebody turned into a public one is a credential that silently
// stopped being one. The flags are re-asserted, not assumed.
func TestEnsureRegistrarRealmClient_RepairsAClientThatWasChanged(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	f.registrar = &keycloakClientRecord{
		ID: "registrar-uuid", ClientID: RegistrarClientID, Enabled: true,
		PublicClient: true, ServiceAccountsEnabled: false, StandardFlowEnabled: true,
	}
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	if _, err := c.EnsureRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if !f.did(http.MethodPut, "/admin/realms/demo/clients/registrar-uuid") {
		t.Fatal("the client was left public")
	}
	f.mu.Lock()
	updated := f.updatedBody
	f.mu.Unlock()
	if updated["publicClient"] != false || updated["serviceAccountsEnabled"] != true {
		t.Errorf("repair did not restore the shape: %v", updated)
	}
}

// A realm with no realm-management client cannot grant anything, and the
// answer has to name the realm: a credential that exists and can do nothing is
// harder to diagnose than one that was never made.
func TestEnsureRegistrarRealmClient_SaysWhichRealmHasNoManagementClient(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	f.noManagement = true
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	_, err := c.EnsureRegistrarRealmClient(context.Background(), "demo")
	if err == nil || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("got %v, want an error naming the realm", err)
	}
}

func TestEnsureRegistrarRealmClient_RefusesARealmNameThatIsAPath(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	if _, err := c.EnsureRegistrarRealmClient(context.Background(), "../master"); err == nil {
		t.Fatal("expected a refusal")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Errorf("nothing should reach Keycloak: %v", f.calls)
	}
}

func TestDeleteRegistrarRealmClient(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	if _, err := c.EnsureRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if !f.did(http.MethodDelete, "/admin/realms/demo/clients/registrar-uuid") {
		t.Fatal("the client was not deleted")
	}
	// Deleting what is already gone is not an error: a retired tenant may be
	// reconciled more than once.
	if err := c.DeleteRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// The credential the director held is removed from a realm, and only that
// one: the registrar's own client is a different client and stays.
func TestTheDirectorsFormerClientIsRetired(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	f.retired = true
	srv := f.server(t)
	c := testAdminClient(srv, "admin", "pw")

	if _, err := c.EnsureRegistrarRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRetiredDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if !f.did(http.MethodDelete, "/admin/realms/demo/clients/director-uuid") {
		t.Fatal("the director's former client was not deleted")
	}
	if f.did(http.MethodDelete, "/admin/realms/demo/clients/registrar-uuid") {
		t.Fatal("the registrar's own client was deleted")
	}

	// Gone already, or never there: nothing to do, and not an error.
	if err := c.DeleteRetiredDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatalf("a realm without the former client: %v", err)
	}
}
