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

// The realm is written when it lacks a setting and left alone when it has them
// all: this runs on every tenant reconcile, and each write is an admin event.
func TestKeycloakAdminClient_UpdateRealmBrowserSecurityHeaders(t *testing.T) {
	t.Parallel()

	// What Keycloak answers: more headers than are set, the locales in another
	// order, and fields this does not touch.
	stale := `{"realm":"demo","loginTheme":"keycloak","browserSecurityHeaders":{"xFrameOptions":"SAMEORIGIN"}}`
	var realm string
	var puts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/realms/master/protocol/openid-connect/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
		case r.URL.Path == "/admin/realms/demo" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(realm))
		case r.URL.Path == "/admin/realms/demo" && r.Method == http.MethodPut:
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
	ctx := context.Background()

	realm = stale
	if err := client.UpdateRealmBrowserSecurityHeaders(ctx, "demo", []string{"en", "de"}); err != nil {
		t.Fatalf("UpdateRealmBrowserSecurityHeaders: %v", err)
	}
	if len(puts) != 1 || puts[0]["loginTheme"] != GentianLoginTheme {
		t.Fatalf("a realm missing the settings was not written: %v", puts)
	}

	// The realm as Keycloak keeps it after that write.
	headers := map[string]any{"extra": "kept"}
	for k, v := range DefaultBrowserSecurityHeaders {
		headers[k] = v
	}
	current, _ := json.Marshal(map[string]any{
		"realm": "demo", "enabled": true, "loginTheme": GentianLoginTheme,
		"browserSecurityHeaders":      headers,
		"internationalizationEnabled": true,
		"supportedLocales":            []string{"de", "en"},
		"defaultLocale":               "en",
	})
	realm = string(current)
	if err := client.UpdateRealmBrowserSecurityHeaders(ctx, "demo", []string{"en", "de"}); err != nil || len(puts) != 1 {
		t.Fatalf("an unchanged realm was written: %v, puts=%d", err, len(puts))
	}

	// A different language order is a different default, and is written.
	if err := client.UpdateRealmBrowserSecurityHeaders(ctx, "demo", []string{"de", "en"}); err != nil || len(puts) != 2 {
		t.Fatalf("a changed default locale was not written: %v, puts=%d", err, len(puts))
	}
}

func TestKeycloakAdminClient_UpdateRealmBrowserSecurityHeaders_EmptyRealm(t *testing.T) {
	t.Parallel()
	client := NewKeycloakAdminClient("http://127.0.0.1:1", "admin", "secret")
	if err := client.UpdateRealmBrowserSecurityHeaders(context.Background(), "", nil); err != nil {
		t.Fatalf("empty realm should no-op: %v", err)
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

// A pack's client scope is removed by name, with its mappers; one of
// Keycloak's own is never removed, whatever a catalog called its scope.
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
				// As Keycloak answers: a scope's attributes are strings.
				out = append(out, map[string]any{"id": "id-" + name, "name": name,
					"attributes": map[string]string{"include.in.token.scope": "true"}})
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

// An app's client is removed by its clientId, only the one asked for; a
// second removal finds nothing, and a client still there afterwards is an
// error whatever the delete answered.
func TestDeleteClient(t *testing.T) {
	clients := map[string]bool{"gentian-wiki": true, "gentian-wiki-extra": true}
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/admin/realms/demo/clients"
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		case r.Method == http.MethodGet && r.URL.Path == base:
			// Loose on purpose: every client whose id starts with the one
			// asked for, which the lookup must not take for an exact match.
			out := []map[string]any{}
			for name := range clients {
				if strings.HasPrefix(name, r.URL.Query().Get("clientId")) {
					out = append(out, map[string]any{"id": "id-" + name, "clientId": name})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/id-"):
			if !refuse {
				delete(clients, strings.TrimPrefix(r.URL.Path, base+"/id-"))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := testAdminClient(srv, "admin", "pw")
	ctx := context.Background()

	existed, err := c.DeleteClient(ctx, "demo", "gentian-wiki")
	if err != nil || !existed || clients["gentian-wiki"] || !clients["gentian-wiki-extra"] {
		t.Fatalf("existed = %v, err = %v, clients = %v", existed, err, clients)
	}
	if existed, err := c.DeleteClient(ctx, "demo", "gentian-wiki"); err != nil || existed {
		t.Fatalf("a second removal: %v %v", existed, err)
	}
	refuse = true
	if _, err := c.DeleteClient(ctx, "demo", "gentian-wiki-extra"); err == nil {
		t.Fatal("a client that is still there was reported deleted")
	}
}

// An account is there when Keycloak answers for its id, gone on a 404, and
// any other answer is an error rather than a guess either way.
func TestUserExistsTellsGoneFromUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		case r.URL.Path == "/admin/realms/demo/users/u-here":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u-here"})
		case r.URL.Path == "/admin/realms/demo/users/u-gone":
			http.NotFound(w, r)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	c := testAdminClient(srv, "admin", "pw")
	ctx := context.Background()
	if ok, err := c.UserExists(ctx, "demo", "u-here"); !ok || err != nil {
		t.Fatalf("u-here: %v %v", ok, err)
	}
	if ok, err := c.UserExists(ctx, "demo", "u-gone"); ok || err != nil {
		t.Fatalf("u-gone: %v %v", ok, err)
	}
	if _, err := c.UserExists(ctx, "other", "u-here"); err == nil {
		t.Fatal("a 503 was taken for an answer")
	}
}
