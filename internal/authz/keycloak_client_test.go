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
