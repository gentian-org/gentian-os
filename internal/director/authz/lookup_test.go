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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModels is an OpenFGA with one store whose models are listed newest
// first, one per page so that paging is exercised.
type fakeModels struct {
	mu     sync.Mutex
	models []map[string]any // newest first
}

func (f *fakeModels) add(m map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append([]map[string]any{m}, f.models...)
}

func (f *fakeModels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/stores":
		_ = json.NewEncoder(w).Encode(map[string]any{"stores": []map[string]any{
			{"id": "store-1", "name": StoreName, "created_at": "2026-01-01T00:00:00Z"},
		}})
	case "/stores/store-1/authorization-models":
		f.mu.Lock()
		defer f.mu.Unlock()
		i := 0
		if tok := r.URL.Query().Get("continuation_token"); tok != "" {
			i = int(tok[0] - '0')
		}
		page := map[string]any{"authorization_models": []map[string]any{}}
		if i < len(f.models) {
			page["authorization_models"] = []map[string]any{f.models[i]}
			if i+1 < len(f.models) {
				page["continuation_token"] = string(rune('0' + i + 1))
			}
		}
		_ = json.NewEncoder(w).Encode(page)
	default:
		http.NotFound(w, r)
	}
}

func embeddedModel(t *testing.T, id string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(modelV1, &m); err != nil {
		t.Fatal(err)
	}
	m["id"] = id
	return m
}

func otherModel(id string) map[string]any {
	return map[string]any{"id": id, "schema_version": "1.1", "type_definitions": []any{
		map[string]any{"type": "user"},
	}}
}

func lookupOptions(t *testing.T, f *fakeModels) Options {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Options{BaseURL: srv.URL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func fastRetry(t *testing.T) {
	t.Helper()
	old := lookupRetry
	lookupRetry = 10 * time.Millisecond
	t.Cleanup(func() { lookupRetry = old })
}

// After a rollback the newest model is one this binary does not know; it
// must decide against its own, further back.
func TestLookupTakesTheEmbeddedModelNotTheNewest(t *testing.T) {
	f := &fakeModels{}
	f.add(otherModel("older"))
	f.add(embeddedModel(t, "mine"))
	f.add(otherModel("newer-1"))
	f.add(otherModel("newer-2"))
	store, model, err := Lookup(context.Background(), lookupOptions(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if store != "store-1" || model != "mine" {
		t.Fatalf("store=%q model=%q, want store-1 mine", store, model)
	}
}

// On an upgrade a reader may look before the operator has written the new
// model; it waits for it rather than settling on the old one.
func TestLookupWaitsForTheOperatorToWriteTheModel(t *testing.T) {
	fastRetry(t)
	f := &fakeModels{}
	f.add(otherModel("previous"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.add(embeddedModel(t, "new"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, model, err := Lookup(ctx, lookupOptions(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if model != "new" {
		t.Fatalf("model=%q, want new", model)
	}
}

func TestLookupGivesUpWhenTheModelNeverArrives(t *testing.T) {
	fastRetry(t)
	f := &fakeModels{}
	f.add(otherModel("previous"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := Lookup(ctx, lookupOptions(t, f))
	if err == nil {
		t.Fatal("Lookup succeeded without the embedded model in the store")
	}
	if !strings.Contains(err.Error(), "does not hold the model this service was built with") {
		t.Fatalf("unclear error: %v", err)
	}
}

func TestLookupHonoursAPinnedModel(t *testing.T) {
	f := &fakeModels{}
	f.add(otherModel("previous"))
	o := lookupOptions(t, f)
	o.ModelID = "pinned"
	store, model, err := Lookup(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if store != "store-1" || model != "pinned" {
		t.Fatalf("store=%q model=%q", store, model)
	}
}
