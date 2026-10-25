/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFGA is the app instance: stores, models and tuples, and every call it
// was made, by store.
type fakeFGA struct {
	mu     sync.Mutex
	token  string
	next   int
	stores map[string]*fakeStore
	calls  []string
}

type fakeStore struct {
	name   string
	models []json.RawMessage
	tuples []fgaTuple
}

func newFakeFGA() *fakeFGA { return &fakeFGA{token: "instance-key", stores: map[string]*fakeStore{}} }

func (f *fakeFGA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, `{"code":"unauthenticated"}`, http.StatusUnauthorized)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.URL.Path == "/stores" && r.Method == http.MethodPost {
		f.next++
		id := fmt.Sprintf("store-%d", f.next)
		f.stores[id] = &fakeStore{name: body["name"].(string)}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/stores/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, op, _ := strings.Cut(rest, "/")
	st, ok := f.stores[id]
	if !ok {
		http.Error(w, `{"code":"store_id_not_found","message":"no such store"}`, http.StatusNotFound)
		return
	}
	if model, ok := body["authorization_model_id"].(string); ok && model != fmt.Sprintf("%s-model-%d", id, len(st.models)) {
		http.Error(w, `{"message":"not the latest model"}`, http.StatusBadRequest)
		return
	}
	keyOf := func(v any) fgaTuple {
		m, _ := v.(map[string]any)
		s := func(k string) string { x, _ := m[k].(string); return x }
		return fgaTuple{User: s("user"), Relation: s("relation"), Object: s("object")}
	}
	has := func(t fgaTuple) bool {
		for _, x := range st.tuples {
			if x == t {
				return true
			}
		}
		return false
	}
	switch {
	case r.Method == http.MethodDelete && op == "":
		delete(f.stores, id)
		w.WriteHeader(http.StatusNoContent)
	case op == "authorization-models":
		raw, _ := json.Marshal(body)
		st.models = append(st.models, raw)
		_ = json.NewEncoder(w).Encode(map[string]string{"authorization_model_id": fmt.Sprintf("%s-model-%d", id, len(st.models))})
	case op == "check":
		if strings.Contains(keyOf(body["tuple_key"]).Object, "undefined") {
			http.Error(w, `{"code":"validation_error","message":"type 'undefined' not found"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": has(keyOf(body["tuple_key"]))})
	case op == "batch-check":
		result := map[string]any{}
		for _, c := range body["checks"].([]any) {
			m := c.(map[string]any)
			result[m["correlation_id"].(string)] = map[string]bool{"allowed": has(keyOf(m["tuple_key"]))}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
	case op == "write":
		for _, kind := range []string{"writes", "deletes"} {
			if group, ok := body[kind].(map[string]any); ok {
				for _, k := range group["tuple_keys"].([]any) {
					t := keyOf(k)
					if kind == "writes" {
						st.tuples = append(st.tuples, t)
						continue
					}
					for i, x := range st.tuples {
						if x == t {
							st.tuples = append(st.tuples[:i], st.tuples[i+1:]...)
							break
						}
					}
				}
			}
		}
		_, _ = io.WriteString(w, "{}")
	case op == "read":
		var out []map[string]fgaTuple
		for _, t := range st.tuples {
			out = append(out, map[string]fgaTuple{"key": t})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tuples": out})
	case op == "list-objects":
		var objects []string
		for _, t := range st.tuples {
			if t.User == body["user"] && t.Relation == body["relation"] && strings.HasPrefix(t.Object, body["type"].(string)+":") {
				objects = append(objects, t.Object)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"objects": objects})
	case op == "list-users":
		_ = json.NewEncoder(w).Encode(map[string]any{"users": []any{
			map[string]any{"object": map[string]string{"type": "user", "id": "acme/1"}},
			map[string]any{"userset": map[string]string{"type": "group", "id": "staff", "relation": "member"}},
			map[string]any{"wildcard": map[string]string{"type": "user"}},
		}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeFGA) storeNamed(name string) (string, *fakeStore) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, st := range f.stores {
		if st.name == name {
			return id, st
		}
	}
	return "", nil
}

// countExact is how many calls were exactly this one.
func (f *fakeFGA) countExact(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *fakeFGA) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// memoryRecorder is the record, in memory.
type memoryRecorder struct {
	mu  sync.Mutex
	ids map[string]StoreIDs
}

func (m *memoryRecorder) Load(context.Context) (map[string]StoreIDs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]StoreIDs{}
	for k, v := range m.ids {
		out[k] = v
	}
	return out, nil
}

func (m *memoryRecorder) Claim(_ context.Context, key string, ids StoreIDs) (StoreIDs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.ids[key]; ok {
		return v, nil
	}
	m.ids[key] = ids
	return ids, nil
}

func (m *memoryRecorder) Save(_ context.Context, key string, ids StoreIDs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids[key] = ids
	return nil
}

func (m *memoryRecorder) Forget(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ids, key)
	return nil
}

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func entry(t *testing.T, tenant, component, key, dsl string) Entry {
	t.Helper()
	model, err := Compile(dsl)
	if err != nil {
		t.Fatal(err)
	}
	return Entry{Tenant: tenant, Component: component, KeyHash: hashOf(key), Model: model}
}

const docModel = `model
  schema 1.1
type user
type doc
  relations
    define viewer: [user]
`

type harness struct {
	fga    *fakeFGA
	rec    *memoryRecorder
	server *Server
	api    http.Handler
}

func newHarness(t *testing.T, entries ...Entry) *harness {
	t.Helper()
	f := newFakeFGA()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &memoryRecorder{ids: map[string]StoreIDs{}}
	fga := NewOpenFGA(srv.URL, f.token)
	s := NewServer(fga, NewStores(fga, rec, log), log)
	s.SetTable(&Table{Stores: entries})
	return &harness{fga: f, rec: rec, server: s, api: s.Handler()}
}

func (h *harness) call(t *testing.T, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		payload = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, payload)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A call without a key, or with one the table does not name, is 401, and
// OpenFGA is never asked.
func TestAKeyTheTableDoesNotNameIs401(t *testing.T) {
	h := newHarness(t, entry(t, "acme", "ledger", "key-a", docModel))
	for _, key := range []string{"", "not-a-key", hashOf("key-a")} {
		code, body := h.call(t, http.MethodPost, "/v1/check", key, CheckRequest{User: "user:1", Relation: "viewer", Object: "doc:1"})
		if code != http.StatusUnauthorized || body["code"] != "unauthenticated" {
			t.Fatalf("key %q: %d %v", key, code, body)
		}
	}
	if n := h.fga.count(""); n != 0 {
		t.Fatalf("OpenFGA was asked %d times", n)
	}
	// A key taken out of the table is refused from the next call on.
	if code, _ := h.call(t, http.MethodGet, "/v1/model", "key-a", nil); code != http.StatusOK {
		t.Fatalf("model: %d", code)
	}
	h.server.SetTable(&Table{})
	if code, _ := h.call(t, http.MethodGet, "/v1/model", "key-a", nil); code != http.StatusUnauthorized {
		t.Fatalf("a key no longer in the table: %d", code)
	}
}

// Each key reaches its own store and no other, and nothing a caller sends
// can name another.
func TestAKeyReachesItsOwnStoreAndNoOther(t *testing.T) {
	h := newHarness(t, entry(t, "acme", "ledger", "key-a", docModel), entry(t, "globex", "ledger", "key-b", docModel))
	tuple := Tuple{User: "user:acme/1", Relation: "viewer", Object: "doc:1"}
	if code, body := h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{tuple}}); code != http.StatusOK {
		t.Fatalf("write: %d %v", code, body)
	}
	if _, body := h.call(t, http.MethodPost, "/v1/check", "key-a", CheckRequest{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object}); body["allowed"] != true {
		t.Fatalf("the writer's own check: %v", body)
	}
	if _, body := h.call(t, http.MethodPost, "/v1/check", "key-b", CheckRequest{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object}); body["allowed"] != false {
		t.Fatalf("another tenant's component sees the tuple: %v", body)
	}
	_, a := h.fga.storeNamed("acme/ledger")
	_, b := h.fga.storeNamed("globex/ledger")
	if a == nil || b == nil || len(a.tuples) != 1 || len(b.tuples) != 0 {
		t.Fatalf("stores: %+v %+v", a, b)
	}
	// A field naming a store, or a model, is not a field of the API.
	for _, body := range []string{
		`{"user":"user:1","relation":"viewer","object":"doc:1","store_id":"store-2"}`,
		`{"user":"user:1","relation":"viewer","object":"doc:1","authorization_model_id":"x"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/check", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer key-a")
		rec := httptest.NewRecorder()
		h.api.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
}

// The store is made on first sight, recorded, and made once; a model that
// changes is written as a new model of the same store, and the tuples stay.
func TestAStoreIsMadeOnceAndANewModelKeepsTheTuples(t *testing.T) {
	first := entry(t, "acme", "ledger", "key-a", docModel)
	h := newHarness(t, first)
	tuple := Tuple{User: "user:acme/1", Relation: "viewer", Object: "doc:1"}
	h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{tuple}})
	h.call(t, http.MethodPost, "/v1/check", "key-a", CheckRequest{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object})
	if n := h.fga.countExact("POST /stores"); n != 1 {
		t.Fatalf("%d stores created, want 1", n)
	}
	id, st := h.fga.storeNamed("acme/ledger")
	if len(st.models) != 1 || h.rec.ids["acme.ledger"].Store != id || h.rec.ids["acme.ledger"].Model == "" {
		t.Fatalf("models %d, record %+v", len(st.models), h.rec.ids)
	}

	changed := entry(t, "acme", "ledger", "key-a", docModel+"    define editor: [user]\n")
	h.server.SetTable(&Table{Stores: []Entry{changed}})
	code, body := h.call(t, http.MethodPost, "/v1/check", "key-a", CheckRequest{User: tuple.User, Relation: "viewer", Object: "doc:1"})
	if code != http.StatusOK || body["allowed"] != true {
		t.Fatalf("after the new model: %d %v", code, body)
	}
	if n := h.fga.countExact("POST /stores"); n != 1 {
		t.Fatalf("a new model made a new store: %d", n)
	}
	if _, st := h.fga.storeNamed("acme/ledger"); len(st.models) != 2 || len(st.tuples) != 1 {
		t.Fatalf("models %d tuples %d", len(st.models), len(st.tuples))
	}
	_, body = h.call(t, http.MethodGet, "/v1/model", "key-a", nil)
	if !strings.Contains(fmt.Sprint(body["dsl"]), "define editor: [user]") || body["id"] != id+"-model-2" {
		t.Fatalf("model: %v", body)
	}
}

// The limits of one call: 50 checks, 100 tuples, pages of 100.
func TestTheLimitsOfOneCall(t *testing.T) {
	h := newHarness(t, entry(t, "acme", "ledger", "key-a", docModel))
	check := CheckRequest{User: "user:1", Relation: "viewer", Object: "doc:1"}
	many := func(n int) []CheckRequest {
		out := make([]CheckRequest, n)
		for i := range out {
			out[i] = check
		}
		return out
	}
	if code, body := h.call(t, http.MethodPost, "/v1/batch-check", "key-a", BatchCheckRequest{Checks: many(MaxBatchChecks)}); code != http.StatusOK || len(body["results"].([]any)) != MaxBatchChecks {
		t.Fatalf("50 checks: %d %v", code, body)
	}
	if code, _ := h.call(t, http.MethodPost, "/v1/batch-check", "key-a", BatchCheckRequest{Checks: many(MaxBatchChecks + 1)}); code != http.StatusBadRequest {
		t.Fatalf("51 checks: %d", code)
	}
	tuples := make([]Tuple, MaxWriteTuples+1)
	for i := range tuples {
		tuples[i] = Tuple{User: fmt.Sprintf("user:%d", i), Relation: "viewer", Object: "doc:1"}
	}
	if code, _ := h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: tuples[:60], Deletes: tuples[60:]}); code != http.StatusBadRequest {
		t.Fatalf("101 tuples: %d", code)
	}
	if code, _ := h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: tuples[:MaxWriteTuples]}); code != http.StatusOK {
		t.Fatalf("100 tuples: %d", code)
	}
	if code, _ := h.call(t, http.MethodPost, "/v1/read", "key-a", ReadRequest{PageSize: MaxPageSize + 1}); code != http.StatusBadRequest {
		t.Fatalf("a page of 101: %d", code)
	}
	if code, body := h.call(t, http.MethodPost, "/v1/read", "key-a", ReadRequest{Tuple: &Tuple{Object: "doc:1"}}); code != http.StatusOK || len(body["tuples"].([]any)) != MaxWriteTuples {
		t.Fatalf("read: %d %v", code, len(body["tuples"].([]any)))
	}
}

// Each key has its rate limit: past the burst, calls are 429, and another
// key is not slowed by it.
func TestEachKeyHasItsOwnRateLimit(t *testing.T) {
	h := newHarness(t, entry(t, "acme", "ledger", "key-a", docModel), entry(t, "globex", "ledger", "key-b", docModel))
	limited := 0
	for range RateBurst + 50 {
		if code, _ := h.call(t, http.MethodGet, "/v1/model", "key-a", nil); code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("no call was limited past the burst")
	}
	if code, _ := h.call(t, http.MethodGet, "/v1/model", "key-b", nil); code != http.StatusOK {
		t.Fatalf("another key was limited: %d", code)
	}
}

// What OpenFGA refuses as the caller's mistake comes back as a 400 with its
// message; the rest of the questions answer in the API's own shapes.
func TestTheQuestionsAnswerInTheAPIsShapes(t *testing.T) {
	h := newHarness(t, entry(t, "acme", "ledger", "key-a", docModel))
	h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "user:1", Relation: "viewer", Object: "doc:1"}}})
	if code, body := h.call(t, http.MethodPost, "/v1/check", "key-a", CheckRequest{User: "user:1", Relation: "viewer", Object: "undefined:1"}); code != http.StatusBadRequest || body["message"] != "type 'undefined' not found" {
		t.Fatalf("a type the model lacks: %d %v", code, body)
	}
	if _, body := h.call(t, http.MethodPost, "/v1/list-objects", "key-a", ListObjectsRequest{User: "user:1", Relation: "viewer", Type: "doc"}); fmt.Sprint(body["objects"]) != "[doc:1]" {
		t.Fatalf("list-objects: %v", body)
	}
	_, body := h.call(t, http.MethodPost, "/v1/list-users", "key-a", ListUsersRequest{Object: "doc:1", Relation: "viewer", UserFilters: []UserFilter{{Type: "user"}}})
	if fmt.Sprint(body["users"]) != "[user:acme/1 group:staff#member user:*]" {
		t.Fatalf("list-users: %v", body)
	}
	if code, _ := h.call(t, http.MethodPost, "/v1/list-users", "key-a", ListUsersRequest{Object: "doc", Relation: "viewer", UserFilters: []UserFilter{{Type: "user"}}}); code != http.StatusBadRequest {
		t.Fatalf("an object without an id: %d", code)
	}
	if code, body := h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Deletes: []Tuple{{User: "user:1", Relation: "viewer", Object: "doc:1"}}}); code != http.StatusOK {
		t.Fatalf("delete: %d %v", code, body)
	}
}

// A store the table says to purge is deleted with its record, once; a table
// that both keeps and purges a store is not one.
func TestAPurgedStoreIsDeletedWithItsRecord(t *testing.T) {
	ledger := entry(t, "acme", "ledger", "key-a", docModel)
	h := newHarness(t, ledger)
	h.call(t, http.MethodGet, "/v1/model", "key-a", nil)
	if _, st := h.fga.storeNamed("acme/ledger"); st == nil {
		t.Fatal("no store")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, TableKey)
	raw, _ := json.Marshal(Table{Purge: []Ref{{Tenant: "acme", Component: "ledger"}}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	Follow(ctx, path, 10*time.Millisecond, h.server, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, st := h.fga.storeNamed("acme/ledger"); st != nil {
		t.Fatal("the purged store is still there")
	}
	if _, ok := h.rec.ids["acme.ledger"]; ok {
		t.Fatal("the purged store is still recorded")
	}
	if code, _ := h.call(t, http.MethodGet, "/v1/model", "key-a", nil); code != http.StatusUnauthorized {
		t.Fatalf("the purge table still names the key: %d", code)
	}
	both, _ := json.Marshal(Table{Stores: []Entry{ledger}, Purge: []Ref{{Tenant: "acme", Component: "ledger"}}})
	if _, err := ParseTable(both); err == nil {
		t.Fatal("a table that keeps and purges one store was taken")
	}
}

// Two arbiters that make a store for one entry at once end with one: the
// record names the first, and the second deletes its own.
func TestTwoArbitersAgreeOnOneStore(t *testing.T) {
	ledger := entry(t, "acme", "ledger", "key-a", docModel)
	f := newFakeFGA()
	srv := httptest.NewServer(f)
	defer srv.Close()
	rec := &memoryRecorder{ids: map[string]StoreIDs{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fga := NewOpenFGA(srv.URL, f.token)
	one, two := NewStores(fga, rec, log), NewStores(fga, rec, log)
	// Both have read the empty record before either created anything.
	if _, err := one.Recorded(context.Background(), "acme", "ledger"); err != nil {
		t.Fatal(err)
	}
	if _, err := two.Recorded(context.Background(), "acme", "ledger"); err != nil {
		t.Fatal(err)
	}
	a, err := one.Resolve(context.Background(), ledger)
	if err != nil {
		t.Fatal(err)
	}
	b, err := two.Resolve(context.Background(), ledger)
	if err != nil {
		t.Fatal(err)
	}
	if a.Store != b.Store {
		t.Fatalf("two stores: %s %s", a.Store, b.Store)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stores) != 1 {
		t.Fatalf("%d stores left, want 1", len(f.stores))
	}
}
