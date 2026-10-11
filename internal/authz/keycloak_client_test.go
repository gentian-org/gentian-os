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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testAdminClient talks to srv through srv's own transport.
//
// The client's default is http.DefaultTransport, shared by every test in the
// package, and httptest.Server.Close calls CloseIdleConnections on it: one
// parallel test finishing broke another's request in flight with
// "connection broken: http: CloseIdleConnections called".
func testAdminClient(srv *httptest.Server, username, password string) *KeycloakAdminClient {
	c := NewKeycloakAdminClient(srv.URL, username, password)
	c.httpClient = srv.Client()
	c.httpClient.Timeout = defaultHTTPTimeout
	return c
}

func TestKeycloakAdminClient_UpdateRealmBrowserSecurityHeaders(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realms/master/protocol/openid-connect/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
		case "/admin/realms/demo":
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := testAdminClient(srv, "admin", "secret")
	if err := client.UpdateRealmBrowserSecurityHeaders(context.Background(), "demo", []string{"en", "de"}); err != nil {
		t.Fatalf("UpdateRealmBrowserSecurityHeaders: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/admin/realms/demo" {
		t.Fatalf("got %s %s, want PUT /admin/realms/demo", gotMethod, gotPath)
	}
}

func TestKeycloakAdminClient_UpdateRealmBrowserSecurityHeaders_EmptyRealm(t *testing.T) {
	t.Parallel()
	client := NewKeycloakAdminClient("http://127.0.0.1:1", "admin", "secret")
	if err := client.UpdateRealmBrowserSecurityHeaders(context.Background(), "", nil); err != nil {
		t.Fatalf("empty realm should no-op: %v", err)
	}
}

func TestEnsureGroup_MergesAttributesInsteadOfReplacingThem(t *testing.T) {
	t.Parallel()

	// A group already carrying an administrator's hand-set roles and the App
	// Store's default-grant marker. EnsureGroup is called with only the keys an
	// ComponentProfile declares, which is what the tenant identity path passes.
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/realms/master/protocol/openid-connect/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
		case r.URL.Path == "/admin/realms/demo/groups" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"g1","name":"gentian:tenant:demo:app:odoo-crm-ce"}]`))
		case r.URL.Path == "/admin/realms/demo/groups/g1" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"g1","name":"gentian:tenant:demo:app:odoo-crm-ce",
				"attributes":{"gentianDefaultGrant":["true"],"custom":["kept"],
				"gentianOdooModules":["stale"]}}`))
		case r.URL.Path == "/admin/realms/demo/groups/g1" && r.Method == http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				t.Fatalf("decode PUT body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	client := testAdminClient(srv, "admin", "secret")
	id, err := client.EnsureGroup(context.Background(), "demo",
		"gentian:tenant:demo:app:odoo-crm-ce",
		map[string][]string{"gentianOdooModules": {"crm"}})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if id != "g1" {
		t.Fatalf("got group id %q, want g1", id)
	}

	attrs, ok := putBody["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("PUT body carried no attributes map: %#v", putBody)
	}
	// Untouched keys survive; the caller's key wins where they collide.
	for key, want := range map[string]string{
		"gentianDefaultGrant": "true",
		"custom":              "kept",
		"gentianOdooModules":  "crm",
	} {
		got, _ := attrs[key].([]any)
		if len(got) != 1 || got[0] != want {
			t.Errorf("attribute %s = %#v, want [%q]", key, attrs[key], want)
		}
	}
}

// The sender name changes and nothing else does: the masked password goes back
// as it came, which Keycloak reads as "keep the stored one"; a realm without
// mail, or already named, is not written at all.
func TestKeycloakAdminClient_UpdateRealmMailSender(t *testing.T) {
	t.Parallel()
	realm := `{"smtpServer":{"host":"smtp.example","from":"noreply@k.example","fromDisplayName":"Gentian","password":"**********"}}`
	var puts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/realms/master/protocol/openid-connect/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
		case r.URL.Path == "/admin/realms/kernel" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(realm))
		case r.URL.Path == "/admin/realms/kernel" && r.Method == http.MethodPut:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			puts = append(puts, body)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	client := testAdminClient(srv, "admin", "secret")

	if err := client.UpdateRealmMailSender(context.Background(), "kernel", "Acme Cloud"); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 1 {
		t.Fatalf("puts = %d", len(puts))
	}
	smtp := puts[0]["smtpServer"].(map[string]any)
	if smtp["fromDisplayName"] != "Acme Cloud" || smtp["password"] != "**********" || smtp["host"] != "smtp.example" || len(puts[0]) != 1 {
		t.Fatalf("put = %v", puts[0])
	}

	if err := client.UpdateRealmMailSender(context.Background(), "kernel", "Gentian"); err != nil || len(puts) != 1 {
		t.Fatalf("an unchanged name was written: %v, puts=%d", err, len(puts))
	}
	realm = `{"smtpServer":{}}`
	if err := client.UpdateRealmMailSender(context.Background(), "kernel", "Acme Cloud"); err != nil || len(puts) != 1 {
		t.Fatalf("a realm without mail was written: %v, puts=%d", err, len(puts))
	}
}

// groupRealm is a realm's groups, as much of the admin API as deleting one
// touches.
func groupRealm(t *testing.T, groups map[string]bool, refuseDelete bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/admin/realms/demo/groups"
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		case r.Method == http.MethodGet && r.URL.Path == base:
			out := []map[string]any{}
			for name := range groups {
				out = append(out, map[string]any{"id": "id-" + name, "name": name})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/id-"):
			if !refuseDelete {
				delete(groups, strings.TrimPrefix(r.URL.Path, base+"/id-"))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Deleting a group removes it, says whether it was there, and can be asked
// again. A group that is still there afterwards is an error whatever the
// delete answered: the caller reports the group gone.
func TestDeleteGroupRemovesTheGroupAndIsRepeatable(t *testing.T) {
	groups := map[string]bool{"gentian:tenant:demo:app:wiki": true, "gentian:tenant:demo:app:drive": true, "gentian:tenant:demo:admins": true}
	c := testAdminClient(groupRealm(t, groups, false), "admin", "pw")

	names, err := c.GroupNames(context.Background(), "demo", "gentian:tenant:demo:app:")
	if err != nil || len(names) != 2 {
		t.Fatalf("GroupNames = %v %v", names, err)
	}
	existed, err := c.DeleteGroup(context.Background(), "demo", "gentian:tenant:demo:app:wiki")
	if err != nil || !existed {
		t.Fatalf("DeleteGroup = %v %v", existed, err)
	}
	if groups["gentian:tenant:demo:app:wiki"] || !groups["gentian:tenant:demo:app:drive"] || !groups["gentian:tenant:demo:admins"] {
		t.Fatalf("groups after the delete: %v", groups)
	}
	existed, err = c.DeleteGroup(context.Background(), "demo", "gentian:tenant:demo:app:wiki")
	if err != nil || existed {
		t.Fatalf("a second DeleteGroup = %v %v", existed, err)
	}

	stuck := map[string]bool{"gentian:tenant:demo:app:wiki": true}
	c = testAdminClient(groupRealm(t, stuck, true), "admin", "pw")
	if _, err := c.DeleteGroup(context.Background(), "demo", "gentian:tenant:demo:app:wiki"); err == nil {
		t.Fatal("a group that is still there was reported deleted")
	}
}

// A pack's client scope is removed by name, with its mappers; one of
// Keycloak's own is never removed, whatever a catalogue called its scope.
func TestDeleteClientScope(t *testing.T) {
	scopes := map[string]bool{"wiki-scope": true, "drive-scope": true, "profile": true}
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/admin/realms/demo/client-scopes"
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		case r.Method == http.MethodGet && r.URL.Path == base:
			out := []map[string]any{}
			for name := range scopes {
				out = append(out, map[string]any{"id": "id-" + name, "name": name})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/id-"):
			if !refuse {
				delete(scopes, strings.TrimPrefix(r.URL.Path, base+"/id-"))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := testAdminClient(srv, "admin", "pw")
	ctx := context.Background()

	existed, err := c.DeleteClientScope(ctx, "demo", "wiki-scope")
	if err != nil || !existed || scopes["wiki-scope"] || !scopes["drive-scope"] {
		t.Fatalf("existed = %v, err = %v, scopes = %v", existed, err, scopes)
	}
	if existed, err := c.DeleteClientScope(ctx, "demo", "wiki-scope"); err != nil || existed {
		t.Fatalf("a second removal: %v %v", existed, err)
	}
	if _, err := c.DeleteClientScope(ctx, "demo", "profile"); err == nil || !scopes["profile"] {
		t.Fatalf("one of Keycloak's own scopes was deleted: err = %v, scopes = %v", err, scopes)
	}
	refuse = true
	if _, err := c.DeleteClientScope(ctx, "demo", "drive-scope"); err == nil {
		t.Fatal("a scope that is still there was reported deleted")
	}
}
