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
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
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

// hashGateway is the part of the admin API a key and a team are written
// through, as the gateway keeps them: a key by alias, as the SHA-256 of the
// key with what it carries; a team by alias with its models and limits.
// objects makes it list whole keys (return_full_object) instead of bare
// hashes; opaque makes it list something that is neither.
type hashGateway struct {
	keys    map[string]string // alias -> hash
	carried map[string]map[string]any
	teams   map[string]map[string]any
	objects bool
	opaque  bool
	calls   []string
}

func (g *hashGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.calls = append(g.calls, r.URL.Path)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	text := func(k string) string { v, _ := body[k].(string); return v }
	switch r.URL.Path {
	case "/key/list":
		keys := []any{}
		alias := r.URL.Query().Get("key_alias")
		if hash, ok := g.keys[alias]; ok {
			switch {
			case g.opaque:
				keys = append(keys, "sk-...abcd")
			case g.objects && r.URL.Query().Get("return_full_object") == "true":
				whole := map[string]any{"token": hash, "key_alias": alias, "team_id": nil, "models": []any{}, "allowed_routes": []any{}, "metadata": map[string]any{}}
				for k, v := range g.carried[alias] {
					whole[k] = v
				}
				keys = append(keys, whole)
			default:
				keys = append(keys, hash)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	case "/key/generate":
		alias := text("key_alias")
		if _, taken := g.keys[alias]; taken {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"alias in use"}`))
			return
		}
		g.keys[alias] = HashKey(text("key"))
		g.carried[alias] = carriedOf(body)
		_, _ = w.Write([]byte(`{}`))
	case "/key/update":
		for alias, hash := range g.keys {
			if hash == HashKey(text("key")) {
				g.carried[alias] = carriedOf(body)
				_, _ = w.Write([]byte(`{}`))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case "/key/delete":
		for _, alias := range body["key_aliases"].([]any) {
			delete(g.keys, alias.(string))
		}
		_, _ = w.Write([]byte(`{}`))
	case "/team/list":
		out := []map[string]any{}
		for _, team := range g.teams {
			out = append(out, team)
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/team/new":
		body["team_id"] = "id-" + text("team_alias")
		g.teams[text("team_alias")] = body
		_, _ = w.Write([]byte(`{}`))
	case "/team/update":
		for alias, team := range g.teams {
			if team["team_id"] == body["team_id"] {
				for k, v := range body {
					team[k] = v
				}
				g.teams[alias] = team
			}
		}
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func carriedOf(body map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"team_id", "models", "allowed_routes", "metadata"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	return out
}

func newHashGateway(t *testing.T) (*hashGateway, *Client) {
	t.Helper()
	g := &hashGateway{keys: map[string]string{}, carried: map[string]map[string]any{}, teams: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	return g, &Client{BaseURL: srv.URL, MasterKey: "master"}
}

func wikiSpec() KeySpec {
	return KeySpec{
		TeamID: "id-demo", Models: []string{EveryTenantGroup}, Routes: AppRoutes,
		Metadata: map[string]string{"tenant": "demo", "app": "wiki"},
	}
}

// An alias names one key. Registering the key it already names does nothing;
// registering another replaces what was there, so that the key made from
// names stops authenticating in the pass that registers the generated one.
func TestEnsureKeyRegistersOnceAndReplacesAnotherKey(t *testing.T) {
	ctx := context.Background()
	g, c := newHashGateway(t)
	g.objects = true
	g.keys["other-wiki"] = HashKey("sk-somebody-elses")

	replaced, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated", wikiSpec())
	if err != nil || replaced {
		t.Fatalf("first registration: replaced = %v, err = %v", replaced, err)
	}
	if got := g.carried["demo-wiki"]; got["team_id"] != "id-demo" || len(got["allowed_routes"].([]any)) != len(AppRoutes) {
		t.Fatalf("the key carries %v", got)
	}
	before := len(g.calls)
	if replaced, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated", wikiSpec()); err != nil || replaced {
		t.Fatalf("second pass: replaced = %v, err = %v", replaced, err)
	}
	if got := g.calls[before:]; len(got) != 1 || got[0] != "/key/list" {
		t.Errorf("a key already registered cost %v, want one look", got)
	}

	// Another key is registered under the alias: it is replaced.
	g.keys["demo-wiki"] = HashKey("sk-previous")
	replaced, err = c.EnsureKey(ctx, "demo-wiki", "sk-generated", wikiSpec())
	if err != nil || !replaced {
		t.Fatalf("replacement: replaced = %v, err = %v", replaced, err)
	}
	if g.keys["demo-wiki"] != HashKey("sk-generated") {
		t.Errorf("the alias names %s, want the generated key", g.keys["demo-wiki"])
	}
	if g.keys["other-wiki"] != HashKey("sk-somebody-elses") {
		t.Error("another alias's key was touched")
	}
}

// A key registered before keys belonged to a team -- no team, every model,
// every route -- is brought to what it should carry in place: the app goes
// on holding the same key, and it is never deleted and registered again.
func TestEnsureKeyMovesARegisteredKeyIntoItsTeamInPlace(t *testing.T) {
	ctx := context.Background()
	for _, objects := range []bool{true, false} {
		g, c := newHashGateway(t)
		g.objects = objects
		g.keys["demo-wiki"] = HashKey("sk-generated")
		replaced, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated", wikiSpec())
		if err != nil || replaced {
			t.Fatalf("objects=%v: replaced = %v, err = %v", objects, replaced, err)
		}
		for _, call := range g.calls {
			if call == "/key/delete" || call == "/key/generate" {
				t.Errorf("objects=%v: the key was %s", objects, call)
			}
		}
		got := g.carried["demo-wiki"]
		if got["team_id"] != "id-demo" || got["metadata"].(map[string]any)["tenant"] != "demo" ||
			len(got["models"].([]any)) != 1 || got["models"].([]any)[0] != EveryTenantGroup {
			t.Errorf("objects=%v: the key carries %v", objects, got)
		}
	}
	// A key whose models changed with its tenant's is updated again.
	g, c := newHashGateway(t)
	g.objects = true
	if _, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated", wikiSpec()); err != nil {
		t.Fatal(err)
	}
	more := wikiSpec()
	more.Models = append(more.Models, ProviderGroup("acme"))
	if _, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated", more); err != nil {
		t.Fatal(err)
	}
	if n := len(g.carried["demo-wiki"]["models"].([]any)); n != 2 {
		t.Errorf("the key names %d model groups, want 2", n)
	}
	// No models is never written as an empty list, which the gateway reads
	// as every model.
	none := wikiSpec()
	none.Models = nil
	if _, err := c.EnsureKey(ctx, "demo-wiki", "sk-generated", none); err != nil {
		t.Fatal(err)
	}
	if m := g.carried["demo-wiki"]["models"].([]any); len(m) != 1 || m[0] != NoModels {
		t.Errorf("a key with no models names %v", m)
	}
}

// A tenant's team is made with the models and limits it is given, is not
// written while it carries them, and is brought back to them -- a limit
// removed is cleared, not left as it was.
func TestEnsureTeam(t *testing.T) {
	ctx := context.Background()
	g, c := newHashGateway(t)
	budget, rpm := int64(50), int64(20)
	want := Team{Models: []string{EveryTenantGroup}, MaxBudget: &budget, BudgetDuration: "30d", RPMLimit: &rpm}
	id, err := c.EnsureTeam(ctx, "demo", want)
	if err != nil || id != "id-demo" {
		t.Fatalf("id = %q, err = %v", id, err)
	}
	if team := g.teams["demo"]; team["max_budget"] != float64(50) || team["budget_duration"] != "30d" || team["tpm_limit"] != nil {
		t.Fatalf("team = %v", team)
	}
	before := len(g.calls)
	if _, err := c.EnsureTeam(ctx, "demo", want); err != nil {
		t.Fatal(err)
	}
	if got := g.calls[before:]; len(got) != 1 || got[0] != "/team/list" {
		t.Errorf("a team that carries what it should cost %v", got)
	}
	// The budget goes; a provider comes.
	plain := Team{Models: []string{EveryTenantGroup, ProviderGroup("acme")}}
	if _, err := c.EnsureTeam(ctx, "demo", plain); err != nil {
		t.Fatal(err)
	}
	team := g.teams["demo"]
	if team["max_budget"] != nil || team["budget_duration"] != nil || team["rpm_limit"] != nil || len(team["models"].([]any)) != 2 {
		t.Fatalf("team = %v", team)
	}
	// A team with no models names none, never an empty list.
	if _, err := c.EnsureTeam(ctx, "demo", Team{}); err != nil {
		t.Fatal(err)
	}
	if m := g.teams["demo"]["models"].([]any); len(m) != 1 || m[0] != NoModels {
		t.Errorf("a team with no models names %v", m)
	}
}

// What a tenant's team names: the models every tenant may use and those of
// the providers it opted in to, without a group that holds no model, and
// the gateway's name for none when nothing is left.
func TestTeamModels(t *testing.T) {
	served := map[string]bool{EveryTenantGroup: true, ProviderGroup("acme"): true}
	for _, c := range []struct {
		providers []string
		served    map[string]bool
		want      string
	}{
		{nil, served, EveryTenantGroup},
		{[]string{"zeta", "acme", "acme"}, served, EveryTenantGroup + " " + ProviderGroup("acme")},
		{[]string{"zeta", "acme"}, nil, EveryTenantGroup + " " + ProviderGroup("acme") + " " + ProviderGroup("zeta")},
		{[]string{"zeta"}, map[string]bool{}, NoModels},
		{nil, map[string]bool{ProviderGroup("acme"): true}, NoModels},
	} {
		if got := strings.Join(TeamModels(c.providers, c.served), " "); got != c.want {
			t.Errorf("TeamModels(%v, %v) = %q, want %q", c.providers, c.served, got, c.want)
		}
	}
}

// The groups that hold a model are read off the claim as the gateway's chart
// writes its configuration from it (scripts/tests/llm_models.py holds the
// chart to the same fixture).
func TestServedGroupsFollowTheChart(t *testing.T) {
	raw, err := os.ReadFile("../../crossplane/tests/unit/schema/valid/cluster-llm-models.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		Spec struct {
			LLM ClaimModels `json:"llm"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &claim); err != nil {
		t.Fatal(err)
	}
	got := claim.Spec.LLM.ServedGroups()
	// The staged provider has no model.
	if len(got) != 2 || !got[EveryTenantGroup] || !got[ProviderGroup("infomaniak")] {
		t.Fatalf("served = %v", got)
	}
	claim.Spec.LLM.GPUAcceleration = false
	if got := claim.Spec.LLM.ServedGroups(); got[EveryTenantGroup] {
		t.Errorf("without GPUs every tenant's group holds a model: %v", got)
	}
	claim.Spec.LLM.Providers[1].Models = claim.Spec.LLM.Providers[0].Models
	if got := claim.Spec.LLM.ServedGroups(); got[EveryTenantGroup] || !got[ProviderGroup("staged")] {
		t.Errorf("a provider with a model is every tenant's, or not its own group's: %v", got)
	}
	claim.Spec.LLM.Providers[1].APIKeyProperty = "infomaniak_api_key"
	if got := claim.Spec.LLM.ServedGroups(); got[EveryTenantGroup] || got[ProviderGroup("staged")] {
		t.Errorf("a provider naming another's token is offered: %v", got)
	}
}

// A listing that does not carry the key's hash cannot be compared with the
// key an app holds. That is an error: read as "another key", it would delete
// and register the app's key on every pass.
func TestEnsureKeyDoesNotReplaceAKeyItCannotCompare(t *testing.T) {
	g, c := newHashGateway(t)
	g.opaque = true
	g.keys["demo-wiki"] = HashKey("sk-generated")
	if _, err := c.EnsureKey(context.Background(), "demo-wiki", "sk-generated", wikiSpec()); err == nil {
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
