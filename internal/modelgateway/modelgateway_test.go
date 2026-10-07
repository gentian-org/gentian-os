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
	if got := VirtualKey("demo", "wiki"); got != "sk-gentian-demo-wiki" {
		t.Errorf("VirtualKey = %q", got)
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
