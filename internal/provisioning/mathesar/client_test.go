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

package mathesar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_ListUsers(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rpc/v0/" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "bootstrap" || pass != "s3cr3t" {
			t.Fatalf("missing/wrong basic auth: %q %q %v", user, pass, ok)
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != "users.list" {
			t.Fatalf("got method %q, want users.list", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[
			{"id":1,"username":"admin","is_superuser":true,"email":"admin@example.org","full_name":"","display_language":"en"}
		]}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "bootstrap", "s3cr3t")
	users, err := client.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "admin" || !users[0].IsSuperuser {
		t.Fatalf("unexpected users: %+v", users)
	}
}

func TestClient_AddUser(t *testing.T) {
	t.Parallel()

	var gotParams map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != "users.add" {
			t.Fatalf("got method %q, want users.add", req.Method)
		}
		params, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(params, &gotParams)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":
			{"id":2,"username":"newadmin","is_superuser":true,"email":"newadmin@example.org","full_name":"","display_language":"en"}
		}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "bootstrap", "s3cr3t")
	user, err := client.AddUser(context.Background(), "newadmin", "newadmin@example.org", true)
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if user.ID != 2 || !user.IsSuperuser {
		t.Fatalf("unexpected user: %+v", user)
	}
	userDef, ok := gotParams["user_def"].(map[string]any)
	if !ok {
		t.Fatalf("params missing user_def: %+v", gotParams)
	}
	if userDef["username"] != "newadmin" || userDef["email"] != "newadmin@example.org" {
		t.Fatalf("unexpected user_def: %+v", userDef)
	}
	if pw, _ := userDef["password"].(string); pw == "" {
		t.Fatalf("expected a generated password, got empty")
	}
}

func TestClient_SetSuperuser_ResendsAllFields(t *testing.T) {
	t.Parallel()

	var gotParams map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != "users.patch_other" {
			t.Fatalf("got method %q, want users.patch_other", req.Method)
		}
		params, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(params, &gotParams)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":
			{"id":3,"username":"alice","is_superuser":false,"email":"alice@example.org","full_name":"Alice","display_language":"en"}
		}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "bootstrap", "s3cr3t")
	existing := User{ID: 3, Username: "alice", Email: "alice@example.org", FullName: "Alice", DisplayLanguage: "en"}
	user, err := client.SetSuperuser(context.Background(), existing, false)
	if err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if user.IsSuperuser {
		t.Fatalf("expected demotion to take effect")
	}
	for _, field := range []string{"username", "email", "full_name", "display_language"} {
		if gotParams[field] == nil || gotParams[field] == "" {
			t.Fatalf("patch_other dropped field %q: %+v", field, gotParams)
		}
	}
}

func TestClient_RPCError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Forbidden"}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "bootstrap", "s3cr3t")
	if _, err := client.ListUsers(context.Background()); err == nil {
		t.Fatalf("expected an error from an RPC error response")
	}
}
