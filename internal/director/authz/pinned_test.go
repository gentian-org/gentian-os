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
	"errors"
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
// first, one per page so that paging is exercised. It counts every listing
// of the stores: a reader must never make one.
type fakeModels struct {
	mu       sync.Mutex
	models   []map[string]any // newest first
	listings int
}

func (f *fakeModels) add(m map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append([]map[string]any{m}, f.models...)
}

func (f *fakeModels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/stores":
		f.listings++
		_ = json.NewEncoder(w).Encode(map[string]any{"stores": []map[string]any{
			{"id": "impostor", "name": StoreName, "created_at": "2025-01-01T00:00:00Z"},
			{"id": "store-1", "name": StoreName, "created_at": "2026-01-01T00:00:00Z"},
		}})
	case r.URL.Path == "/stores/store-1/authorization-models":
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
	case strings.HasPrefix(r.URL.Path, "/stores/store-1/authorization-models/"):
		id := strings.TrimPrefix(r.URL.Path, "/stores/store-1/authorization-models/")
		for _, m := range f.models {
			if m["id"] == id {
				_ = json.NewEncoder(w).Encode(map[string]any{"authorization_model": m})
				return
			}
		}
		http.NotFound(w, r)
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

func pinnedOptions(t *testing.T, f *fakeModels, model string) Options {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Options{
		BaseURL: srv.URL, StoreID: "store-1", ModelID: model,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func fastRetry(t *testing.T) {
	t.Helper()
	old := pinRetry
	pinRetry = 10 * time.Millisecond
	t.Cleanup(func() { pinRetry = old })
}

// Without the operator's record a reader does not start, and in particular
// does not go looking for a store by its name.
func TestPinnedRefusesToStartWithoutTheRecord(t *testing.T) {
	f := &fakeModels{}
	f.add(embeddedModel(t, "mine"))
	for _, o := range []Options{
		pinnedOptions(t, f, ""),
		func() Options { o := pinnedOptions(t, f, "mine"); o.StoreID = ""; return o }(),
	} {
		if _, _, err := Pinned(context.Background(), o); !errors.Is(err, ErrNotPinned) {
			t.Fatalf("err = %v, want ErrNotPinned", err)
		}
	}
	if f.listings != 0 {
		t.Fatalf("the stores were listed %d times", f.listings)
	}
}

func TestPinnedTakesTheRecordedModel(t *testing.T) {
	f := &fakeModels{}
	f.add(embeddedModel(t, "mine"))
	f.add(otherModel("newer"))
	store, model, err := Pinned(context.Background(), pinnedOptions(t, f, "mine"))
	if err != nil {
		t.Fatal(err)
	}
	if store != "store-1" || model != "mine" {
		t.Fatalf("store=%q model=%q, want store-1 mine", store, model)
	}
	if f.listings != 0 {
		t.Fatalf("the stores were listed %d times", f.listings)
	}
}

// After a rollback the recorded model is one this binary does not know; it
// must decide against its own, further back in the same store.
func TestPinnedTakesTheEmbeddedModelNotTheRecordedOne(t *testing.T) {
	f := &fakeModels{}
	f.add(otherModel("older"))
	f.add(embeddedModel(t, "mine"))
	f.add(otherModel("newer-1"))
	f.add(otherModel("newer-2"))
	store, model, err := Pinned(context.Background(), pinnedOptions(t, f, "newer-2"))
	if err != nil {
		t.Fatal(err)
	}
	if store != "store-1" || model != "mine" {
		t.Fatalf("store=%q model=%q, want store-1 mine", store, model)
	}
}

// On an upgrade a reader may read the record before the operator has written
// the new model; it waits for it rather than settling on the old one.
func TestPinnedWaitsForTheOperatorToWriteTheModel(t *testing.T) {
	fastRetry(t)
	f := &fakeModels{}
	f.add(otherModel("previous"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.add(embeddedModel(t, "new"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, model, err := Pinned(ctx, pinnedOptions(t, f, "previous"))
	if err != nil {
		t.Fatal(err)
	}
	if model != "new" {
		t.Fatalf("model=%q, want new", model)
	}
}

func TestPinnedGivesUpWhenTheModelNeverArrives(t *testing.T) {
	fastRetry(t)
	f := &fakeModels{}
	f.add(otherModel("previous"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := Pinned(ctx, pinnedOptions(t, f, "previous"))
	if err == nil {
		t.Fatal("Pinned succeeded without the embedded model in the store")
	}
	if !strings.Contains(err.Error(), "does not hold the model this service was built with") {
		t.Fatalf("unclear error: %v", err)
	}
}
