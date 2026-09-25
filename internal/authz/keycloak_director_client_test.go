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

	// director is nil until it is created.
	director *keycloakClientRecord
	// granted is the set of realm-management roles the service account holds.
	granted map[string]bool
	// calls records method+path, for asserting what was and was not done.
	calls []string
	// createdBody is the representation the client was created with.
	createdBody map[string]any
	// updatedBody is the representation it was re-asserted with, if any.
	updatedBody map[string]any
	// noManagement makes the realm answer with no realm-management client.
	noManagement bool
}

func newFakeRealm() *fakeRealm { return &fakeRealm{granted: map[string]bool{}} }

// allRealmManagementRoles is what a real realm offers; the director must take
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
			case DirectorClientID:
				f.mu.Lock()
				d := f.director
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
			default:
				_, _ = w.Write([]byte(`[]`))
			}

		case r.URL.Path == "/admin/realms/demo/clients" && r.Method == http.MethodPost:
			f.mu.Lock()
			_ = json.Unmarshal(body, &f.createdBody)
			f.director = &keycloakClientRecord{
				ID: "director-uuid", ClientID: DirectorClientID, Enabled: true,
				PublicClient: false, ServiceAccountsEnabled: true, StandardFlowEnabled: false,
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)

		case r.URL.Path == "/admin/realms/demo/clients/director-uuid" && r.Method == http.MethodPut:
			f.mu.Lock()
			_ = json.Unmarshal(body, &f.updatedBody)
			if f.director != nil {
				f.director.Enabled = true
				f.director.PublicClient = false
				f.director.ServiceAccountsEnabled = true
				f.director.StandardFlowEnabled = false
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.URL.Path == "/admin/realms/demo/clients/director-uuid/service-account-user":
			_, _ = w.Write([]byte(`{"id":"sa-uuid","username":"service-account-gentian-director-admin"}`))

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

		case r.URL.Path == "/admin/realms/demo/clients/director-uuid/client-secret":
			_, _ = w.Write([]byte(`{"type":"secret","value":"s3cr3t"}`))

		case r.URL.Path == "/admin/realms/demo/clients/director-uuid" && r.Method == http.MethodDelete:
			f.mu.Lock()
			f.director = nil
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

func TestEnsureDirectorRealmClient_CreatesAConfidentialServiceAccount(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := NewKeycloakAdminClient(srv.URL, "admin", "pw")

	secret, err := c.EnsureDirectorRealmClient(context.Background(), "demo")
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

// The list of roles IS the security statement: everything the director can do
// in a realm is on it, so anything else turning up is a change nobody made
// deliberately.
func TestEnsureDirectorRealmClient_TakesOnlyItsOwnRoles(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := NewKeycloakAdminClient(srv.URL, "admin", "pw")

	if _, err := c.EnsureDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, r := range f.heldRoles() {
		held[r] = true
	}
	for _, want := range directorRealmRoles {
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
			t.Fatalf("the director holds %q", r)
		}
	}
}

// A second pass must not re-create, re-grant or rotate anything. This is what
// a reconciler does on every loop.
func TestEnsureDirectorRealmClient_IsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := NewKeycloakAdminClient(srv.URL, "admin", "pw")

	if _, err := c.EnsureDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	secret, err := c.EnsureDirectorRealmClient(context.Background(), "demo")
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
	if f.did(http.MethodPut, "/admin/realms/demo/clients/director-uuid") {
		t.Error("a client already in the right shape was rewritten")
	}
}

// A client somebody turned into a public one is a credential that silently
// stopped being one. The flags are re-asserted, not assumed.
func TestEnsureDirectorRealmClient_RepairsAClientThatWasChanged(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	f.director = &keycloakClientRecord{
		ID: "director-uuid", ClientID: DirectorClientID, Enabled: true,
		PublicClient: true, ServiceAccountsEnabled: false, StandardFlowEnabled: true,
	}
	srv := f.server(t)
	c := NewKeycloakAdminClient(srv.URL, "admin", "pw")

	if _, err := c.EnsureDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if !f.did(http.MethodPut, "/admin/realms/demo/clients/director-uuid") {
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
func TestEnsureDirectorRealmClient_SaysWhichRealmHasNoManagementClient(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	f.noManagement = true
	srv := f.server(t)
	c := NewKeycloakAdminClient(srv.URL, "admin", "pw")

	_, err := c.EnsureDirectorRealmClient(context.Background(), "demo")
	if err == nil || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("got %v, want an error naming the realm", err)
	}
}

func TestEnsureDirectorRealmClient_RefusesARealmNameThatIsAPath(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := NewKeycloakAdminClient(srv.URL, "admin", "pw")

	if _, err := c.EnsureDirectorRealmClient(context.Background(), "../master"); err == nil {
		t.Fatal("expected a refusal")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Errorf("nothing should reach Keycloak: %v", f.calls)
	}
}

func TestDeleteDirectorRealmClient(t *testing.T) {
	t.Parallel()
	f := newFakeRealm()
	srv := f.server(t)
	c := NewKeycloakAdminClient(srv.URL, "admin", "pw")

	if _, err := c.EnsureDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if !f.did(http.MethodDelete, "/admin/realms/demo/clients/director-uuid") {
		t.Fatal("the client was not deleted")
	}
	// Deleting what is already gone is not an error: a retired tenant may be
	// reconciled more than once.
	if err := c.DeleteDirectorRealmClient(context.Background(), "demo"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
