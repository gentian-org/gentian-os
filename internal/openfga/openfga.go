/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package openfga is OpenFGA's HTTP API and nothing more: plain requests,
// one credential, every path under the server's root, and every store named
// by the caller. What a store means, which store a caller may reach and
// which model a decision is made against are the callers' to say: the
// kernel's client in internal/director/authz and the arbiter's in
// internal/arbiter.
package openfga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxWrite is OpenFGA's default limit on tuples per write request.
const MaxWrite = 100

// Client asks one OpenFGA server.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New is a client of the server at baseURL. token, when set, is sent as a
// bearer credential on every request; hc carries the caller's timeout.
func New(baseURL, token string, hc *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: hc}
}

// Tuple is a relationship as OpenFGA's API spells it. An empty field is left
// out, which is how a partial tuple filters a read.
type Tuple struct {
	User     string `json:"user,omitempty"`
	Relation string `json:"relation,omitempty"`
	Object   string `json:"object,omitempty"`
}

// Error is a refusal from OpenFGA, kept apart from a transport error so that
// a caller can tell "there is no such store" or "the request was malformed"
// from "OpenFGA did not answer".
type Error struct {
	// Path is the request's path, without its query.
	Path string
	Code int
	// Body is what OpenFGA answered, trimmed.
	Body string
}

func (e *Error) Error() string {
	return fmt.Sprintf("openfga %s: status %d: %s", e.Path, e.Code, e.Body)
}

// Message is what OpenFGA said, without the status: its error body is
// {"code": ..., "message": ...}. A body that is not that is returned whole.
func (e *Error) Message() string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(e.Body), &body) == nil && body.Message != "" {
		return body.Message
	}
	return e.Body
}

// IsStatus reports OpenFGA refusing with code.
func IsStatus(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Do sends body, JSON-encoded unless nil, to path under the server root and
// decodes a 2xx answer into out, which may be nil. Any other status is an
// *Error.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := c.request(ctx, method, path, payload)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openfga %s: %w", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return &Error{Path: req.URL.Path, Code: resp.StatusCode, Body: readBody(resp)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// request is a request with the credential, and a JSON content type when it
// carries a body.
func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.TrimSpace(string(b))
}

// Check asks whether user has relation on object in store, against model.
//
// It is stricter than Do: only a 200 is an answer, and a failure is a plain
// error naming the check, not an *Error. All three fields are sent, empty or
// not.
func (c *Client) Check(ctx context.Context, store, model, user, relation, object string) (bool, error) {
	var body struct {
		TupleKey struct {
			User     string `json:"user"`
			Relation string `json:"relation"`
			Object   string `json:"object"`
		} `json:"tuple_key"`
		AuthorizationModelID string `json:"authorization_model_id"`
	}
	body.TupleKey.User, body.TupleKey.Relation, body.TupleKey.Object = user, relation, object
	body.AuthorizationModelID = model
	payload, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := c.request(ctx, http.MethodPost, "/stores/"+store+"/check", bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("openfga check: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("openfga check: status %d: %s", resp.StatusCode, readBody(resp))
	}
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("openfga check: %w", err)
	}
	return out.Allowed, nil
}

// Store is a store as OpenFGA lists it.
type Store struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateStore makes a store called name and returns its id as OpenFGA
// answered it, empty or not.
func (c *Client) CreateStore(ctx context.Context, name string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := c.Do(ctx, http.MethodPost, "/stores", map[string]string{"name": name}, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// GetStore reads one store.
func (c *Client) GetStore(ctx context.Context, id string) (Store, error) {
	var s Store
	err := c.Do(ctx, http.MethodGet, "/stores/"+id, nil, &s)
	return s, err
}

// DeleteStore removes a store with every tuple and model in it.
func (c *Client) DeleteStore(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/stores/"+id, nil, nil)
}

// StoresNamed lists every store, page by page, and returns those called
// name.
func (c *Client) StoresNamed(ctx context.Context, name string) ([]Store, error) {
	var out []Store
	token := ""
	for {
		var page struct {
			Stores            []Store `json:"stores"`
			ContinuationToken string  `json:"continuation_token"`
		}
		q := url.Values{"page_size": {"100"}}
		if token != "" {
			q.Set("continuation_token", token)
		}
		if err := c.Do(ctx, http.MethodGet, "/stores?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Stores {
			if s.Name == name {
				out = append(out, s)
			}
		}
		if token = page.ContinuationToken; token == "" {
			return out, nil
		}
	}
}

// WriteModel writes model to store and returns the id OpenFGA gave it, empty
// or not. Models are immutable: every write is a new version.
func (c *Client) WriteModel(ctx context.Context, store string, model any) (string, error) {
	var written struct {
		ID string `json:"authorization_model_id"`
	}
	if err := c.Do(ctx, http.MethodPost, "/stores/"+store+"/authorization-models", model, &written); err != nil {
		return "", err
	}
	return written.ID, nil
}

// ReadModel is one model of a store, as OpenFGA holds it. id goes into the
// path as given.
func (c *Client) ReadModel(ctx context.Context, store, id string) (json.RawMessage, error) {
	var out struct {
		Model json.RawMessage `json:"authorization_model"`
	}
	if err := c.Do(ctx, http.MethodGet, "/stores/"+store+"/authorization-models/"+id, nil, &out); err != nil {
		return nil, err
	}
	return out.Model, nil
}

// ReadModels is one page of a store's models, newest first, from the
// continuation token of the previous page, and the token for the next.
func (c *Client) ReadModels(ctx context.Context, store string, pageSize int, token string) ([]map[string]any, string, error) {
	path := fmt.Sprintf("/stores/%s/authorization-models?page_size=%d", store, pageSize)
	if token != "" {
		path += "&continuation_token=" + url.QueryEscape(token)
	}
	var page struct {
		Models            []map[string]any `json:"authorization_models"`
		ContinuationToken string           `json:"continuation_token"`
	}
	if err := c.Do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, "", err
	}
	return page.Models, page.ContinuationToken, nil
}

// WriteOptions shape a write.
type WriteOptions struct {
	// Model is the authorization model the tuples are held to; empty sends
	// none and OpenFGA takes the store's latest.
	Model string
	// OnDuplicate and OnMissing, when set, are OpenFGA's on_duplicate for
	// the writes and on_missing for the deletes ("ignore" or "error").
	OnDuplicate string
	OnMissing   string
}

// Write deletes and writes tuples in store. Each request is atomic and
// carries either deletes or writes, at most MaxWrite of them; deletes go
// first, so that an interruption leaves less access rather than more.
func (c *Client) Write(ctx context.Context, store string, writes, deletes []Tuple, o WriteOptions) error {
	send := func(kind string, tuples []Tuple, onConflict, policy string) error {
		for len(tuples) > 0 {
			n := min(len(tuples), MaxWrite)
			group := map[string]any{"tuple_keys": tuples[:n]}
			if policy != "" {
				group[onConflict] = policy
			}
			body := map[string]any{kind: group}
			if o.Model != "" {
				body["authorization_model_id"] = o.Model
			}
			if err := c.Do(ctx, http.MethodPost, "/stores/"+store+"/write", body, nil); err != nil {
				return err
			}
			tuples = tuples[n:]
		}
		return nil
	}
	if err := send("deletes", deletes, "on_missing", o.OnMissing); err != nil {
		return err
	}
	return send("writes", writes, "on_duplicate", o.OnDuplicate)
}

// ReadOptions shape a read.
type ReadOptions struct {
	// Filter is sent as the tuple_key, its empty fields left out; nil sends
	// none, which reads the whole store.
	Filter   *Tuple
	PageSize int
	// StopAtEmptyPage ends the read at a page without tuples even when
	// OpenFGA hands out a continuation token with it.
	StopAtEmptyPage bool
}

// Read returns every tuple of store that matches the filter, following the
// pages to the end.
func (c *Client) Read(ctx context.Context, store string, o ReadOptions) ([]Tuple, error) {
	var out []Tuple
	token := ""
	for {
		body := map[string]any{"page_size": o.PageSize}
		if o.Filter != nil {
			body["tuple_key"] = *o.Filter
		}
		if token != "" {
			body["continuation_token"] = token
		}
		var page struct {
			Tuples []struct {
				Key Tuple `json:"key"`
			} `json:"tuples"`
			ContinuationToken string `json:"continuation_token"`
		}
		if err := c.Do(ctx, http.MethodPost, "/stores/"+store+"/read", body, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Tuples {
			out = append(out, t.Key)
		}
		if token = page.ContinuationToken; token == "" || (o.StopAtEmptyPage && len(page.Tuples) == 0) {
			return out, nil
		}
	}
}

// Change is one entry of a store's changelog.
type Change struct {
	Tuple     Tuple
	Write     bool // false is a delete
	Timestamp time.Time
}

// Changes returns a page of store's changelog for one object type (all
// types when empty), from the continuation token of the previous page, and
// the token for the next.
func (c *Client) Changes(ctx context.Context, store, objectType, token string) ([]Change, string, error) {
	q := url.Values{"page_size": {"100"}}
	if objectType != "" {
		q.Set("type", objectType)
	}
	if token != "" {
		q.Set("continuation_token", token)
	}
	var page struct {
		Changes []struct {
			Key       Tuple     `json:"tuple_key"`
			Operation string    `json:"operation"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"changes"`
		ContinuationToken string `json:"continuation_token"`
	}
	if err := c.Do(ctx, http.MethodGet, "/stores/"+store+"/changes?"+q.Encode(), nil, &page); err != nil {
		return nil, "", err
	}
	out := make([]Change, 0, len(page.Changes))
	for _, ch := range page.Changes {
		out = append(out, Change{Tuple: ch.Key, Write: ch.Operation == "TUPLE_OPERATION_WRITE", Timestamp: ch.Timestamp})
	}
	return out, page.ContinuationToken, nil
}
