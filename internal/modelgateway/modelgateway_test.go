/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package modelgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gateway is the admin API as the LiteLLM of 2026-10 answers it, observed
// against a real one: /key/list by alias lists a key until it is deleted,
// /key/info goes on answering 200 for a deleted key, /key/delete answers 404
// for a key that is not there, and /team/delete takes ids.
type gateway struct {
	keys     map[string]bool
	teams    map[string]string
	deleted  map[string]bool
	calls    []string
	stickKey bool
	down     bool
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	g.calls = append(g.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer master" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if g.down {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"detail":"upstream"}`))
		return
	}
	var body struct {
		KeyAliases []string `json:"key_aliases"`
		TeamIDs    []string `json:"team_ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/key/list":
		keys := []string{}
		if g.keys[r.URL.Query().Get("key_alias")] {
			keys = append(keys, "hash")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys, "total_count": len(keys)})
	case "/key/info":
		// What makes /key/info useless for this: 200 for a deleted key.
		if g.keys[r.URL.Query().Get("key")] || g.deleted[r.URL.Query().Get("key")] {
			_, _ = w.Write([]byte(`{"key":"x","info":{}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case "/key/delete":
		found := false
		for _, alias := range body.KeyAliases {
			if g.keys[alias] {
				found = true
				if !g.stickKey {
					delete(g.keys, alias)
					g.deleted[alias] = true
				}
			}
		}
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"No keys found"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted_keys": body.KeyAliases})
	case "/team/list":
		out := []map[string]any{}
		for alias, id := range g.teams {
			out = append(out, map[string]any{"team_alias": alias, "team_id": id})
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/team/delete":
		for _, id := range body.TeamIDs {
			for alias, have := range g.teams {
				if have == id {
					delete(g.teams, alias)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted_teams": body.TeamIDs})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newGateway(t *testing.T) (*gateway, *Client) {
	t.Helper()
	g := &gateway{keys: map[string]bool{"demo-wiki": true, "demo-drive": true}, teams: map[string]string{"demo": "t-1", "other": "t-2"}, deleted: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	return g, &Client{BaseURL: srv.URL, MasterKey: "master"}
}

func TestTheNamesOfAKeyAndATeam(t *testing.T) {
	// The key made from names is spelled only to be recognised and removed.
	if got := LegacyKey("demo", "wiki"); got != "sk-gentian-demo-wiki" {
		t.Errorf("LegacyKey = %q", got)
	}
	if got := KeyAlias("demo", "wiki"); got != "demo-wiki" {
		t.Errorf("KeyAlias = %q", got)
	}
	if got := TeamAlias("demo"); got != "demo" {
		t.Errorf("TeamAlias = %q", got)
	}
}

// A key is removed by its alias, known gone only when the gateway no longer
// lists it, and removing one that is not there is nothing.
func TestDeleteKey(t *testing.T) {
	g, c := newGateway(t)
	ctx := context.Background()

	existed, err := c.DeleteKey(ctx, "demo-wiki")
	if err != nil || !existed {
		t.Fatalf("existed = %v, err = %v", existed, err)
	}
	if g.keys["demo-wiki"] || !g.keys["demo-drive"] {
		t.Fatalf("keys = %v, want wiki's gone and drive's kept", g.keys)
	}
	// Deleted is not listed, whatever /key/info goes on saying.
	if exists, err := c.KeyExists(ctx, "demo-wiki"); err != nil || exists {
		t.Errorf("a deleted key reads as registered: %v, %v", exists, err)
	}
	existed, err = c.DeleteKey(ctx, "demo-wiki")
	if err != nil || existed {
		t.Errorf("a second removal: existed = %v, err = %v", existed, err)
	}
	for _, call := range g.calls {
		if strings.Contains(call, "/key/info") {
			t.Errorf("existence was asked of /key/info, which answers for deleted keys: %v", g.calls)
		}
	}

	// A key the gateway says it deleted and still lists is an error.
	g.stickKey = true
	if _, err := c.DeleteKey(ctx, "demo-drive"); err == nil || !strings.Contains(err.Error(), "still lists") {
		t.Errorf("err = %v, want the key reported as still there", err)
	}
	// A gateway that does not answer is not a key that is not there.
	g.down = true
	if _, err := c.DeleteKey(ctx, "demo-drive"); err == nil {
		t.Error("an unreachable gateway was read as nothing to delete")
	}
	if _, err := c.KeyExists(ctx, "demo-drive"); err == nil {
		t.Error("an error answer was read as no such key")
	}
}

func TestDeleteTeam(t *testing.T) {
	g, c := newGateway(t)
	ctx := context.Background()
	existed, err := c.DeleteTeam(ctx, "demo")
	if err != nil || !existed {
		t.Fatalf("existed = %v, err = %v", existed, err)
	}
	if _, there := g.teams["demo"]; there || g.teams["other"] != "t-2" {
		t.Fatalf("teams = %v", g.teams)
	}
	if existed, err := c.DeleteTeam(ctx, "demo"); err != nil || existed {
		t.Errorf("a second removal: existed = %v, err = %v", existed, err)
	}
	g.down = true
	if _, err := c.DeleteTeam(ctx, "other"); err == nil {
		t.Error("an unreachable gateway was read as no such team")
	}
}

// hashGateway is the part of the admin API a key is registered through, as
// the gateway keeps keys: by alias, each as the SHA-256 of the key. objects
// makes it list key objects instead of bare hashes; opaque makes it list
// something that is neither.
type hashGateway struct {
	keys    map[string]string // alias -> hash
	objects bool
	opaque  bool
	calls   []string
}

func (g *hashGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.calls = append(g.calls, r.URL.Path)
	var body struct {
		Key        string   `json:"key"`
		KeyAlias   string   `json:"key_alias"`
		KeyAliases []string `json:"key_aliases"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/key/list":
		keys := []any{}
		if hash, ok := g.keys[r.URL.Query().Get("key_alias")]; ok {
			switch {
			case g.opaque:
				keys = append(keys, "sk-...abcd")
			case g.objects:
				keys = append(keys, map[string]any{"token": hash, "key_alias": r.URL.Query().Get("key_alias")})
			default:
				keys = append(keys, hash)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	case "/key/generate":
		if _, taken := g.keys[body.KeyAlias]; taken {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"alias in use"}`))
			return
		}
		g.keys[body.KeyAlias] = HashKey(body.Key)
		_, _ = w.Write([]byte(`{}`))
	case "/key/delete":
		for _, alias := range body.KeyAliases {
			delete(g.keys, alias)
		}
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newHashGateway(t *testing.T) (*hashGateway, *Client) {
	t.Helper()
	g := &hashGateway{keys: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	return g, &Client{BaseURL: srv.URL, MasterKey: "master"}
}

// An alias names one key. Registering the key it already names does nothing;
// registering another replaces what was there, so that the key made from
// names stops authenticating in the pass that registers the generated one.
func TestEnsureKeyRegistersOnceAndReplacesAnotherKey(t *testing.T) {
	ctx := context.Background()
	for _, objects := range []bool{false, true} {
		g, c := newHashGateway(t)
		g.objects = objects
		g.keys["other-wiki"] = HashKey("sk-somebody-elses")

		replaced, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated")
		if err != nil || replaced {
			t.Fatalf("first registration: replaced = %v, err = %v", replaced, err)
		}
		before := len(g.calls)
		if replaced, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated"); err != nil || replaced {
			t.Fatalf("second pass: replaced = %v, err = %v", replaced, err)
		}
		if got := g.calls[before:]; len(got) != 1 || got[0] != "/key/list" {
			t.Errorf("a key already registered cost %v, want one look", got)
		}

		// The key made from names is registered: it is replaced.
		g.keys["demo-wiki"] = HashKey(LegacyKey("demo", "wiki"))
		if is, _ := c.KeyIs(ctx, "demo-wiki", LegacyKey("demo", "wiki")); !is {
			t.Fatal("the key made from names is not recognised")
		}
		replaced, err = c.EnsureKey(ctx, "demo-wiki", "sk-generated")
		if err != nil || !replaced {
			t.Fatalf("replacement: replaced = %v, err = %v", replaced, err)
		}
		if g.keys["demo-wiki"] != HashKey("sk-generated") {
			t.Errorf("the alias names %s, want the generated key", g.keys["demo-wiki"])
		}
		if is, _ := c.KeyIs(ctx, "demo-wiki", LegacyKey("demo", "wiki")); is {
			t.Error("the key made from names is still registered")
		}
		if g.keys["other-wiki"] != HashKey("sk-somebody-elses") {
			t.Error("another alias's key was touched")
		}
	}
}

// A listing that does not carry the key's hash cannot be compared with the
// key an app holds. That is an error: read as "another key", it would delete
// and register the app's key on every pass.
func TestEnsureKeyDoesNotReplaceAKeyItCannotCompare(t *testing.T) {
	g, c := newHashGateway(t)
	g.opaque = true
	g.keys["demo-wiki"] = HashKey("sk-generated")
	if _, err := c.EnsureKey(context.Background(), "demo-wiki", "sk-generated"); err == nil {
		t.Fatal("a key that could not be compared was passed over without an error")
	}
	for _, call := range g.calls {
		if call != "/key/list" {
			t.Errorf("the gateway was asked %s for a key that could not be compared", call)
		}
	}
	// It is still known to exist, which is all a removal needs.
	if exists, err := c.KeyExists(context.Background(), "demo-wiki"); err != nil || !exists {
		t.Errorf("exists = %v, err = %v", exists, err)
	}
}
