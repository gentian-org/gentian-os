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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openFGA is the app instance of OpenFGA, asked over its HTTP API in the
// style of the kernel's client (internal/director/authz): plain requests,
// one credential, every path under the server's root. The arbiter is the
// only holder of that credential, and it names a store in every path from
// the key a caller presented, never from anything the caller sent.
type openFGA struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewOpenFGA is a client of the app instance at baseURL, with its preshared
// key.
func NewOpenFGA(baseURL, token string) *openFGA {
	return &openFGA{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// fgaError is a refusal from OpenFGA, kept apart from a transport error: a
// 400 is the caller's (a type the model does not have, a malformed tuple),
// anything else is the arbiter's to report as its own failure.
type fgaError struct {
	path string
	code int
	body string
}

func (e *fgaError) Error() string {
	return fmt.Sprintf("openfga %s: status %d: %s", e.path, e.code, e.body)
}

// message is what OpenFGA said, without the status: its error body is
// {"code": ..., "message": ...}, and only the message is the caller's to see.
func (e *fgaError) message() string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(e.body), &body) == nil && body.Message != "" {
		return body.Message
	}
	return e.body
}

func isStatus(err error, code int) bool {
	var fe *fgaError
	return errors.As(err, &fe) && fe.code == code
}

func (c *openFGA) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openfga %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &fgaError{path: path, code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// createStore makes a store and returns its id.
func (c *openFGA) createStore(ctx context.Context, name string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/stores", map[string]string{"name": name}, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("openfga created store %q without an id", name)
	}
	return created.ID, nil
}

// deleteStore removes a store with every tuple and model in it. A store
// that is not there is gone already.
func (c *openFGA) deleteStore(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/stores/"+id, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// writeModel writes a model to a store and returns its id. Models are
// immutable and versioned: every write is a new one, and the store's tuples
// stay.
func (c *openFGA) writeModel(ctx context.Context, storeID string, model json.RawMessage) (string, error) {
	var written struct {
		ID string `json:"authorization_model_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/stores/"+storeID+"/authorization-models", model, &written); err != nil {
		return "", err
	}
	if written.ID == "" {
		return "", errors.New("openfga wrote a model without an id")
	}
	return written.ID, nil
}

// writeTuples writes and deletes tuples for the platform, in transactions of
// at most MaxWriteTuples, the way the platform's own writes are made: a tuple
// that is there already is not an error, nor is one that is gone already. A
// component's own writes are not made this way (Server.write): a component
// is told what OpenFGA says.
func (c *openFGA) writeTuples(ctx context.Context, storeID, model string, writes, deletes []Tuple) error {
	send := func(kind string, tuples []Tuple, option map[string]string) error {
		for len(tuples) > 0 {
			n := min(len(tuples), MaxWriteTuples)
			keys := make([]fgaTuple, 0, n)
			for _, t := range tuples[:n] {
				keys = append(keys, toFGA(t))
			}
			group := map[string]any{"tuple_keys": keys}
			for k, v := range option {
				group[k] = v
			}
			body := map[string]any{kind: group}
			if model != "" {
				body["authorization_model_id"] = model
			}
			if err := c.do(ctx, http.MethodPost, storePath(storeID, "/write"), body, nil); err != nil {
				return err
			}
			tuples = tuples[n:]
		}
		return nil
	}
	if err := send("deletes", deletes, map[string]string{"on_missing": "ignore"}); err != nil {
		return err
	}
	return send("writes", writes, map[string]string{"on_duplicate": "ignore"})
}

// readAll is every tuple of a store matching a partial tuple, all pages.
func (c *openFGA) readAll(ctx context.Context, storeID string, filter fgaTuple) ([]Tuple, error) {
	var out []Tuple
	token := ""
	for {
		body := map[string]any{"page_size": MaxPageSize}
		if filter != (fgaTuple{}) {
			body["tuple_key"] = filter
		}
		if token != "" {
			body["continuation_token"] = token
		}
		var page struct {
			Tuples []struct {
				Key fgaTuple `json:"key"`
			} `json:"tuples"`
			ContinuationToken string `json:"continuation_token"`
		}
		if err := c.do(ctx, http.MethodPost, storePath(storeID, "/read"), body, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Tuples {
			out = append(out, Tuple(t.Key))
		}
		if token = page.ContinuationToken; token == "" || len(page.Tuples) == 0 {
			return out, nil
		}
	}
}

// readModel is one model of a store, as OpenFGA holds it.
func (c *openFGA) readModel(ctx context.Context, storeID, model string) (json.RawMessage, error) {
	var out struct {
		Model json.RawMessage `json:"authorization_model"`
	}
	if err := c.do(ctx, http.MethodGet, storePath(storeID, "/authorization-models/"+model), nil, &out); err != nil {
		return nil, err
	}
	return out.Model, nil
}

// storePath is a path under one store.
func storePath(storeID, rest string) string { return "/stores/" + storeID + rest }
