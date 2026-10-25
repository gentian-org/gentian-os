/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package arbiter is the one way into the app instance of OpenFGA.
//
// A component that declares requires.services.authorization is given a key
// (the operator's Secret authorization-<component>), and the arbiter knows
// each key only by its hash, from the table the operator writes: which
// tenant's which component it opens, and the model that store is decided
// against. Every call names its store by the key it carries and by nothing
// else -- no path, field or header a caller sends can name another -- and
// is answered from that store and its latest model. The arbiter is the only
// holder of the instance's own credential, which opens every store.
//
// The API is OpenFGA's, narrowed: the questions and the tuple writes a
// component needs, with the store and the model taken from the key. Each
// key has a rate limit, so that no component can slow another's decisions.
package arbiter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/openfga/language/pkg/go/transformer"
	"golang.org/x/time/rate"
)

// The limits of one call and one key.
const (
	// MaxBatchChecks is the most checks one batch-check carries.
	MaxBatchChecks = 50
	// MaxWriteTuples is the most tuples one write carries, writes and
	// deletes together: OpenFGA's own limit for one transaction.
	MaxWriteTuples = 100
	// MaxPageSize is the largest page a read returns; DefaultPageSize is
	// what a read that names none gets.
	MaxPageSize     = 100
	DefaultPageSize = 50
	// MaxContextualTuples bounds what one question may add for itself.
	MaxContextualTuples = 20
	// RatePerSecond and RateBurst are each key's limit.
	RatePerSecond = 100
	RateBurst     = 200
	// maxBody bounds a request body.
	maxBody = 1 << 20
	// maxField bounds a user, relation, object or type.
	maxField = 512
)

// Tuple is a relation between a user and an object, as a caller writes it:
// "user:acme/123" is "viewer" of "doc:42".
type Tuple struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

// CheckRequest asks whether user has relation on object, with tuples that
// hold for this question only.
type CheckRequest struct {
	User             string  `json:"user"`
	Relation         string  `json:"relation"`
	Object           string  `json:"object"`
	ContextualTuples []Tuple `json:"contextualTuples,omitempty"`
}

type CheckResponse struct {
	Allowed bool `json:"allowed"`
}

type BatchCheckRequest struct {
	Checks []CheckRequest `json:"checks"`
}

// BatchCheckResult is one answer of a batch, in the order of the checks: an
// error is that check's alone, and the others are answered.
type BatchCheckResult struct {
	Allowed bool   `json:"allowed"`
	Error   string `json:"error,omitempty"`
}

type BatchCheckResponse struct {
	Results []BatchCheckResult `json:"results"`
}

type ListObjectsRequest struct {
	User             string  `json:"user"`
	Relation         string  `json:"relation"`
	Type             string  `json:"type"`
	ContextualTuples []Tuple `json:"contextualTuples,omitempty"`
}

type ListObjectsResponse struct {
	Objects []string `json:"objects"`
}

// UserFilter names the kind of user a list-users asks for: a type, or the
// members of a type's relation ("group", "member" is group:...#member).
type UserFilter struct {
	Type     string `json:"type"`
	Relation string `json:"relation,omitempty"`
}

type ListUsersRequest struct {
	Object           string       `json:"object"`
	Relation         string       `json:"relation"`
	UserFilters      []UserFilter `json:"userFilters"`
	ContextualTuples []Tuple      `json:"contextualTuples,omitempty"`
}

// ListUsersResponse names each user as a tuple would: "user:acme/123",
// "group:staff#member", or "user:*" for everyone of a type.
type ListUsersResponse struct {
	Users []string `json:"users"`
}

// ReadRequest reads the tuples matching a partial tuple: any field may be
// left out, and an object of only a type ("doc:") matches every object of
// it. A read with no tuple reads the whole store.
type ReadRequest struct {
	Tuple             *Tuple `json:"tuple,omitempty"`
	PageSize          int    `json:"pageSize,omitempty"`
	ContinuationToken string `json:"continuationToken,omitempty"`
}

type ReadResponse struct {
	Tuples            []Tuple `json:"tuples"`
	ContinuationToken string  `json:"continuationToken,omitempty"`
}

type WriteRequest struct {
	Writes  []Tuple `json:"writes,omitempty"`
	Deletes []Tuple `json:"deletes,omitempty"`
}

// ModelResponse is the model in force: its id in the store, its DSL and its
// JSON as OpenFGA holds it.
type ModelResponse struct {
	ID    string          `json:"id"`
	DSL   string          `json:"dsl"`
	Model json.RawMessage `json:"model"`
}

// ErrorResponse is every refusal.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Server answers the API.
type Server struct {
	fga    *openFGA
	stores *Stores
	log    *slog.Logger

	table atomic.Pointer[keyed]

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

// keyed is a table indexed by the hash of each key.
type keyed struct {
	byHash map[string]Entry
}

func NewServer(fga *openFGA, stores *Stores, log *slog.Logger) *Server {
	s := &Server{fga: fga, stores: stores, log: log, limiters: map[string]*rate.Limiter{}}
	s.SetTable(&Table{})
	return s
}

// SetTable puts a table in force. A key the new table does not name is
// refused from the next call on.
func (s *Server) SetTable(t *Table) {
	k := &keyed{byHash: make(map[string]Entry, len(t.Stores))}
	for _, e := range t.Stores {
		k.byHash[e.KeyHash] = e
	}
	s.table.Store(k)
}

// Handler is the API's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/check", s.call(s.check))
	mux.HandleFunc("POST /v1/batch-check", s.call(s.batchCheck))
	mux.HandleFunc("POST /v1/list-objects", s.call(s.listObjects))
	mux.HandleFunc("POST /v1/list-users", s.call(s.listUsers))
	mux.HandleFunc("POST /v1/read", s.call(s.read))
	mux.HandleFunc("POST /v1/write", s.call(s.write))
	mux.HandleFunc("GET /v1/model", s.call(s.model))
	return mux
}

// refusal is an answer that is not a 200.
type refusal struct {
	status  int
	code    string
	message string
}

func (r *refusal) Error() string { return r.message }

func badRequest(format string, args ...any) *refusal {
	return &refusal{status: http.StatusBadRequest, code: "invalid_request", message: fmt.Sprintf(format, args...)}
}

// handler is one route's work: the store and model the key resolved to, the
// request; the answer or a refusal.
type handler func(ctx context.Context, ids StoreIDs, e Entry, r *http.Request) (any, error)

// call authenticates the key, applies its rate limit, resolves its store
// and hands the rest to h.
func (s *Server) call(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			writeRefusal(w, &refusal{status: http.StatusUnauthorized, code: "unauthenticated", message: "a key is required"})
			return
		}
		sum := sha256.Sum256([]byte(key))
		hash := hex.EncodeToString(sum[:])
		e, known := s.table.Load().byHash[hash]
		if !known {
			writeRefusal(w, &refusal{status: http.StatusUnauthorized, code: "unauthenticated", message: "the key opens no store"})
			return
		}
		if !s.limiter(hash).Allow() {
			w.Header().Set("Retry-After", "1")
			writeRefusal(w, &refusal{status: http.StatusTooManyRequests, code: "rate_limited",
				message: fmt.Sprintf("more than %d calls a second", RatePerSecond)})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		ids, err := s.stores.Resolve(r.Context(), e)
		if err != nil {
			s.log.ErrorContext(r.Context(), "a store could not be resolved", "store", e.StoreName(), "error", err.Error())
			writeRefusal(w, &refusal{status: http.StatusBadGateway, code: "unavailable", message: "the store is not available"})
			return
		}
		out, err := h(r.Context(), ids, e, r)
		if err != nil {
			s.answerError(w, r, e, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// limiter is the rate limit of one key.
func (s *Server) limiter(hash string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.limiters[hash]
	if !ok {
		l = rate.NewLimiter(RatePerSecond, RateBurst)
		s.limiters[hash] = l
	}
	return l
}

// answerError says what went wrong: the caller's mistake as OpenFGA put it,
// anything else as the arbiter's own failure, without OpenFGA's words.
func (s *Server) answerError(w http.ResponseWriter, r *http.Request, e Entry, err error) {
	var ref *refusal
	if errors.As(err, &ref) {
		writeRefusal(w, ref)
		return
	}
	var fe *fgaError
	if errors.As(err, &fe) && fe.code == http.StatusBadRequest {
		writeRefusal(w, &refusal{status: http.StatusBadRequest, code: "invalid_request", message: fe.message()})
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeRefusal(w, &refusal{status: http.StatusRequestEntityTooLarge, code: "too_large", message: "the request is too large"})
		return
	}
	s.log.ErrorContext(r.Context(), "a call failed", "store", e.StoreName(), "path", r.URL.Path, "error", err.Error())
	writeRefusal(w, &refusal{status: http.StatusBadGateway, code: "unavailable", message: "the store did not answer"})
}

func writeRefusal(w http.ResponseWriter, r *refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Code: r.code, Message: r.message})
}

func decode(r *http.Request, into any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return badRequest("the request is not one: %v", err)
	}
	return nil
}

// field holds one name to what OpenFGA could take, before it is asked.
func field(name, v string, required bool) error {
	if v == "" {
		if required {
			return badRequest("%s is required", name)
		}
		return nil
	}
	if len(v) > maxField || strings.ContainsAny(v, " \t\r\n") {
		return badRequest("%s is not one OpenFGA can hold", name)
	}
	return nil
}

func (t Tuple) valid(what string) error {
	if err := field(what+".user", t.User, true); err != nil {
		return err
	}
	if err := field(what+".relation", t.Relation, true); err != nil {
		return err
	}
	return field(what+".object", t.Object, true)
}

// fgaTuple is a tuple as OpenFGA's API spells it.
type fgaTuple struct {
	User     string `json:"user,omitempty"`
	Relation string `json:"relation,omitempty"`
	Object   string `json:"object,omitempty"`
}

func toFGA(t Tuple) fgaTuple { return fgaTuple(t) }

func contextual(tuples []Tuple) (map[string]any, error) {
	if len(tuples) == 0 {
		return nil, nil
	}
	if len(tuples) > MaxContextualTuples {
		return nil, badRequest("at most %d contextual tuples", MaxContextualTuples)
	}
	keys := make([]fgaTuple, 0, len(tuples))
	for i, t := range tuples {
		if err := t.valid(fmt.Sprintf("contextualTuples[%d]", i)); err != nil {
			return nil, err
		}
		keys = append(keys, toFGA(t))
	}
	return map[string]any{"tuple_keys": keys}, nil
}

func (c CheckRequest) body(model string) (map[string]any, error) {
	for _, f := range [][2]string{{"user", c.User}, {"relation", c.Relation}, {"object", c.Object}} {
		if err := field(f[0], f[1], true); err != nil {
			return nil, err
		}
	}
	body := map[string]any{
		"authorization_model_id": model,
		"tuple_key":              fgaTuple{User: c.User, Relation: c.Relation, Object: c.Object},
	}
	ctxTuples, err := contextual(c.ContextualTuples)
	if err != nil {
		return nil, err
	}
	if ctxTuples != nil {
		body["contextual_tuples"] = ctxTuples
	}
	return body, nil
}

func (s *Server) check(ctx context.Context, ids StoreIDs, _ Entry, r *http.Request) (any, error) {
	var req CheckRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	body, err := req.body(ids.Model)
	if err != nil {
		return nil, err
	}
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := s.fga.do(ctx, http.MethodPost, storePath(ids.Store, "/check"), body, &out); err != nil {
		return nil, err
	}
	return CheckResponse{Allowed: out.Allowed}, nil
}

func (s *Server) batchCheck(ctx context.Context, ids StoreIDs, _ Entry, r *http.Request) (any, error) {
	var req BatchCheckRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if len(req.Checks) == 0 || len(req.Checks) > MaxBatchChecks {
		return nil, badRequest("a batch carries 1 to %d checks", MaxBatchChecks)
	}
	checks := make([]map[string]any, 0, len(req.Checks))
	for i, c := range req.Checks {
		body, err := c.body("")
		if err != nil {
			return nil, badRequest("checks[%d]: %v", i, err)
		}
		delete(body, "authorization_model_id")
		body["correlation_id"] = fmt.Sprint(i)
		checks = append(checks, body)
	}
	var out struct {
		Result map[string]struct {
			Allowed bool `json:"allowed"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"result"`
	}
	payload := map[string]any{"authorization_model_id": ids.Model, "checks": checks}
	if err := s.fga.do(ctx, http.MethodPost, storePath(ids.Store, "/batch-check"), payload, &out); err != nil {
		return nil, err
	}
	results := make([]BatchCheckResult, len(req.Checks))
	for i := range req.Checks {
		got, ok := out.Result[fmt.Sprint(i)]
		switch {
		case !ok:
			results[i] = BatchCheckResult{Error: "not answered"}
		case got.Error != nil:
			results[i] = BatchCheckResult{Error: got.Error.Message}
		default:
			results[i] = BatchCheckResult{Allowed: got.Allowed}
		}
	}
	return BatchCheckResponse{Results: results}, nil
}

func (s *Server) listObjects(ctx context.Context, ids StoreIDs, _ Entry, r *http.Request) (any, error) {
	var req ListObjectsRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	for _, f := range [][2]string{{"user", req.User}, {"relation", req.Relation}, {"type", req.Type}} {
		if err := field(f[0], f[1], true); err != nil {
			return nil, err
		}
	}
	body := map[string]any{
		"authorization_model_id": ids.Model,
		"user":                   req.User, "relation": req.Relation, "type": req.Type,
	}
	ctxTuples, err := contextual(req.ContextualTuples)
	if err != nil {
		return nil, err
	}
	if ctxTuples != nil {
		body["contextual_tuples"] = ctxTuples
	}
	var out struct {
		Objects []string `json:"objects"`
	}
	if err := s.fga.do(ctx, http.MethodPost, storePath(ids.Store, "/list-objects"), body, &out); err != nil {
		return nil, err
	}
	if out.Objects == nil {
		out.Objects = []string{}
	}
	return ListObjectsResponse{Objects: out.Objects}, nil
}

func (s *Server) listUsers(ctx context.Context, ids StoreIDs, _ Entry, r *http.Request) (any, error) {
	var req ListUsersRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := field("object", req.Object, true); err != nil {
		return nil, err
	}
	if err := field("relation", req.Relation, true); err != nil {
		return nil, err
	}
	objType, objID, ok := strings.Cut(req.Object, ":")
	if !ok || objType == "" || objID == "" {
		return nil, badRequest("object is <type>:<id>")
	}
	if len(req.UserFilters) == 0 {
		return nil, badRequest("userFilters names at least one type")
	}
	filters := make([]map[string]string, 0, len(req.UserFilters))
	for i, f := range req.UserFilters {
		if err := field(fmt.Sprintf("userFilters[%d].type", i), f.Type, true); err != nil {
			return nil, err
		}
		if err := field(fmt.Sprintf("userFilters[%d].relation", i), f.Relation, false); err != nil {
			return nil, err
		}
		filter := map[string]string{"type": f.Type}
		if f.Relation != "" {
			filter["relation"] = f.Relation
		}
		filters = append(filters, filter)
	}
	body := map[string]any{
		"authorization_model_id": ids.Model,
		"object":                 map[string]string{"type": objType, "id": objID},
		"relation":               req.Relation,
		"user_filters":           filters,
	}
	if ctxTuples, err := contextual(req.ContextualTuples); err != nil {
		return nil, err
	} else if ctxTuples != nil {
		// list-users takes the tuples as a list, not under tuple_keys.
		body["contextual_tuples"] = ctxTuples["tuple_keys"]
	}
	var out struct {
		Users []struct {
			Object *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"object"`
			Userset *struct {
				Type     string `json:"type"`
				ID       string `json:"id"`
				Relation string `json:"relation"`
			} `json:"userset"`
			Wildcard *struct {
				Type string `json:"type"`
			} `json:"wildcard"`
		} `json:"users"`
	}
	if err := s.fga.do(ctx, http.MethodPost, storePath(ids.Store, "/list-users"), body, &out); err != nil {
		return nil, err
	}
	users := make([]string, 0, len(out.Users))
	for _, u := range out.Users {
		switch {
		case u.Object != nil:
			users = append(users, u.Object.Type+":"+u.Object.ID)
		case u.Userset != nil:
			users = append(users, u.Userset.Type+":"+u.Userset.ID+"#"+u.Userset.Relation)
		case u.Wildcard != nil:
			users = append(users, u.Wildcard.Type+":*")
		}
	}
	return ListUsersResponse{Users: users}, nil
}

func (s *Server) read(ctx context.Context, ids StoreIDs, _ Entry, r *http.Request) (any, error) {
	var req ReadRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	switch {
	case req.PageSize == 0:
		req.PageSize = DefaultPageSize
	case req.PageSize < 0 || req.PageSize > MaxPageSize:
		return nil, badRequest("pageSize is 1 to %d", MaxPageSize)
	}
	body := map[string]any{"page_size": req.PageSize}
	if req.ContinuationToken != "" {
		body["continuation_token"] = req.ContinuationToken
	}
	if req.Tuple != nil {
		for _, f := range [][2]string{{"tuple.user", req.Tuple.User}, {"tuple.relation", req.Tuple.Relation}, {"tuple.object", req.Tuple.Object}} {
			if err := field(f[0], f[1], false); err != nil {
				return nil, err
			}
		}
		if *req.Tuple != (Tuple{}) {
			body["tuple_key"] = toFGA(*req.Tuple)
		}
	}
	return s.readPage(ctx, ids.Store, body)
}

// readPage is one page of a read, as the caller sees it.
func (s *Server) readPage(ctx context.Context, storeID string, body map[string]any) (ReadResponse, error) {
	var out struct {
		Tuples []struct {
			Key fgaTuple `json:"key"`
		} `json:"tuples"`
		ContinuationToken string `json:"continuation_token"`
	}
	if err := s.fga.do(ctx, http.MethodPost, storePath(storeID, "/read"), body, &out); err != nil {
		return ReadResponse{}, err
	}
	resp := ReadResponse{Tuples: make([]Tuple, 0, len(out.Tuples)), ContinuationToken: out.ContinuationToken}
	for _, t := range out.Tuples {
		resp.Tuples = append(resp.Tuples, Tuple(t.Key))
	}
	return resp, nil
}

func (s *Server) write(ctx context.Context, ids StoreIDs, _ Entry, r *http.Request) (any, error) {
	var req WriteRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n := len(req.Writes) + len(req.Deletes)
	if n == 0 || n > MaxWriteTuples {
		return nil, badRequest("a write carries 1 to %d tuples, writes and deletes together", MaxWriteTuples)
	}
	body := map[string]any{"authorization_model_id": ids.Model}
	for name, tuples := range map[string][]Tuple{"writes": req.Writes, "deletes": req.Deletes} {
		if len(tuples) == 0 {
			// OpenFGA refuses an empty list where it takes one.
			continue
		}
		keys := make([]fgaTuple, 0, len(tuples))
		for i, t := range tuples {
			if err := t.valid(fmt.Sprintf("%s[%d]", name, i)); err != nil {
				return nil, err
			}
			keys = append(keys, toFGA(t))
		}
		body[name] = map[string]any{"tuple_keys": keys}
	}
	if err := s.fga.do(ctx, http.MethodPost, storePath(ids.Store, "/write"), body, nil); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (s *Server) model(_ context.Context, ids StoreIDs, e Entry, _ *http.Request) (any, error) {
	dsl, err := transformer.TransformJSONStringToDSL(string(e.Model))
	if err != nil {
		return nil, fmt.Errorf("render the model of %s: %w", e.StoreName(), err)
	}
	return ModelResponse{ID: ids.Model, DSL: *dsl, Model: e.Model}, nil
}
