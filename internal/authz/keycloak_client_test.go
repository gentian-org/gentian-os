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
	"net/http"
	"net/http/httptest"
	"testing"
)

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

	client := NewKeycloakAdminClient(srv.URL, "admin", "secret")
	if err := client.UpdateRealmBrowserSecurityHeaders(context.Background(), "demo"); err != nil {
		t.Fatalf("UpdateRealmBrowserSecurityHeaders: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/admin/realms/demo" {
		t.Fatalf("got %s %s, want PUT /admin/realms/demo", gotMethod, gotPath)
	}
}

func TestKeycloakAdminClient_UpdateRealmBrowserSecurityHeaders_EmptyRealm(t *testing.T) {
	t.Parallel()
	client := NewKeycloakAdminClient("http://127.0.0.1:1", "admin", "secret")
	if err := client.UpdateRealmBrowserSecurityHeaders(context.Background(), ""); err != nil {
		t.Fatalf("empty realm should no-op: %v", err)
	}
}

func TestEnsureGroup_MergesAttributesInsteadOfReplacingThem(t *testing.T) {
	t.Parallel()

	// A group already carrying an administrator's hand-set roles and the App
	// Store's default-grant marker. EnsureGroup is called with only the keys an
	// AppProfile declares, which is what the tenant identity path passes.
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

	client := NewKeycloakAdminClient(srv.URL, "admin", "secret")
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
