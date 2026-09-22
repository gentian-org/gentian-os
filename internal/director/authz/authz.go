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

// Package authz asks OpenFGA whether a caller may do something, and records
// the answer.
//
// The director never decides: it names a relation and an object and OpenFGA
// answers (security principle 2). What this package adds is the two things a
// policy enforcement point owes the rest of the platform — every decision is
// logged with the request id that joins it to the issuer's event and to the
// commit it allowed (principle 7), and every identifier crosses into OpenFGA
// through one mapping, so the director, the gateway and the tuple writer can
// never disagree about what a user or a group is called.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ErrInvalidID is returned for an identifier OpenFGA could not store.
var ErrInvalidID = errors.New("identifier cannot be represented in OpenFGA")

// Checker answers one authorization question.
type Checker interface {
	// Check reports whether user has relation on object. requestID is logged
	// with the decision and is not sent to OpenFGA.
	Check(ctx context.Context, requestID, user, relation, object string) (bool, error)
}

// encode maps an external identifier onto OpenFGA's id alphabet. OpenFGA
// reserves ':' (type separator) and '#' (relation separator) and rejects
// whitespace. Keycloak uses ':' in group names (gentian:tenant:demo:admins)
// and in federated user ids (f:<provider>:<id>), so ':' becomes '/', which
// neither uses. Anything else that cannot be stored is refused, not repaired:
// two distinct names must never encode to the same id.
func encode(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, "#/ \t\r\n") {
		return "", fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	return strings.ReplaceAll(s, ":", "/"), nil
}

// User returns the OpenFGA user for a token subject.
func User(sub string) (string, error) {
	id, err := encode(sub)
	if err != nil {
		return "", err
	}
	return "user:" + id, nil
}

// Group returns the OpenFGA object for a Keycloak group name such as
// gentian:tenant:demo:admins.
func Group(name string) (string, error) {
	id, err := encode(name)
	if err != nil {
		return "", err
	}
	return "group:" + id, nil
}

// GroupFromPath returns the OpenFGA object for a Keycloak group path such as
// /gentian:tenant:demo:admins. Nested groups are not part of the platform's
// vocabulary and are refused.
func GroupFromPath(path string) (string, error) {
	return Group(strings.TrimPrefix(path, "/"))
}

// Tenant, Cluster, Session and CatalogueEntry name the objects the director
// checks against. Tenant and cluster names are DNS labels and need no
// encoding; they are validated where they enter.
func Tenant(name string) string  { return "tenant:" + name }
func Cluster(name string) string { return "cluster:" + name }

// Session returns the object a revocation is recorded against.
func Session(sid string) (string, error) {
	id, err := encode(sid)
	if err != nil {
		return "", err
	}
	return "session:" + id, nil
}

// CatalogueEntry returns the object for a store coordinate <catalogue>/<app>.
// The coordinate's own '/' is the one place the separator is legitimate.
func CatalogueEntry(coordinate string) (string, error) {
	cat, app, ok := strings.Cut(coordinate, "/")
	if !ok || cat == "" || app == "" || strings.ContainsAny(coordinate, ":# \t\r\n") || strings.Contains(app, "/") {
		return "", fmt.Errorf("%w: coordinate %q", ErrInvalidID, coordinate)
	}
	return "catalogue_entry:" + coordinate, nil
}

// OpenFGA is a Checker backed by an OpenFGA server.
type OpenFGA struct {
	baseURL string
	token   string
	storeID string
	// modelID pins the authorization model. A check against "whatever is
	// latest" changes meaning when a model is written, which is not something
	// a decision log can explain afterwards.
	modelID string
	http    *http.Client
	log     *slog.Logger
	now     func() time.Time
}

// Options configures an OpenFGA checker.
type Options struct {
	BaseURL  string
	APIToken string
	StoreID  string
	ModelID  string
	Logger   *slog.Logger
	Client   *http.Client
	Now      func() time.Time
}

// NewOpenFGA returns a checker. The store id is required; the model id is
// required too, for the reason on the field.
func NewOpenFGA(o Options) (*OpenFGA, error) {
	if o.BaseURL == "" || o.StoreID == "" {
		return nil, errors.New("authz: OpenFGA URL and store id are required")
	}
	if o.ModelID == "" {
		return nil, errors.New("authz: authorization model id is required: decisions must be made against a pinned model")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &OpenFGA{
		baseURL: strings.TrimRight(o.BaseURL, "/"), token: o.APIToken,
		storeID: o.StoreID, modelID: o.ModelID,
		http: o.Client, log: o.Logger, now: o.Now,
	}, nil
}

type checkRequest struct {
	TupleKey struct {
		User     string `json:"user"`
		Relation string `json:"relation"`
		Object   string `json:"object"`
	} `json:"tuple_key"`
	AuthorizationModelID string         `json:"authorization_model_id"`
	Context              map[string]any `json:"context,omitempty"`
}

// Check implements Checker. The current time is always supplied as context:
// it is a runtime fact, the one thing conditions such as grant_valid need that
// the store cannot hold.
//
// An error is a denial. The caller gets (false, err) and must not proceed;
// there is no mode in which an unreachable OpenFGA lets a write through.
func (c *OpenFGA) Check(ctx context.Context, requestID, user, relation, object string) (allowed bool, err error) {
	start := c.now()
	defer func() {
		attrs := []any{
			"request_id", requestID, "user", user, "relation", relation, "object", object,
			"allowed", allowed, "model", c.modelID, "duration_ms", c.now().Sub(start).Milliseconds(),
		}
		if err != nil {
			c.log.ErrorContext(ctx, "authz decision failed", append(attrs, "error", err.Error())...)
			return
		}
		c.log.InfoContext(ctx, "authz decision", attrs...)
	}()

	var body checkRequest
	body.TupleKey.User, body.TupleKey.Relation, body.TupleKey.Object = user, relation, object
	body.AuthorizationModelID = c.modelID
	body.Context = map[string]any{"current_time": c.now().UTC().Format(time.RFC3339)}
	payload, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/stores/"+c.storeID+"/check", bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("openfga check: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("openfga check: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("openfga check: %w", err)
	}
	return out.Allowed, nil
}

// Tuple is one stored relationship.
type Tuple struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
	// Condition makes the tuple hold only while a model condition does.
	Condition *Condition `json:"condition,omitempty"`
}

// Condition names a model condition and the part of its context that is stored
// with the tuple; the rest arrives with each Check.
type Condition struct {
	Name    string         `json:"name"`
	Context map[string]any `json:"context,omitempty"`
}

// Read returns the stored tuples matching a filter. Any field may be empty,
// within what OpenFGA accepts: an object may be a bare type ("group:") when a
// user is given. Pages are followed to the end.
func (c *OpenFGA) Read(ctx context.Context, filter Tuple) ([]Tuple, error) {
	var out []Tuple
	token := ""
	for {
		body := map[string]any{"page_size": 100}
		key := map[string]string{}
		if filter.User != "" {
			key["user"] = filter.User
		}
		if filter.Relation != "" {
			key["relation"] = filter.Relation
		}
		if filter.Object != "" {
			key["object"] = filter.Object
		}
		body["tuple_key"] = key
		if token != "" {
			body["continuation_token"] = token
		}
		var page struct {
			Tuples []struct {
				Key Tuple `json:"key"`
			} `json:"tuples"`
			ContinuationToken string `json:"continuation_token"`
		}
		if err := c.post(ctx, "/read", body, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Tuples {
			out = append(out, t.Key)
		}
		if token = page.ContinuationToken; token == "" {
			return out, nil
		}
	}
}

// maxWrite is OpenFGA's default limit on tuples per write request.
const maxWrite = 100

// Write adds and removes tuples. Each request is atomic; a change larger than
// one request is split, deletes first, so that an interruption leaves less
// access rather than more.
func (c *OpenFGA) Write(ctx context.Context, writes, deletes []Tuple) error {
	send := func(field string, tuples []Tuple) error {
		for len(tuples) > 0 {
			n := min(len(tuples), maxWrite)
			body := map[string]any{
				"authorization_model_id": c.modelID,
				field:                    map[string]any{"tuple_keys": tuples[:n]},
			}
			if err := c.post(ctx, "/write", body, nil); err != nil {
				return err
			}
			tuples = tuples[n:]
		}
		return nil
	}
	bare := make([]Tuple, len(deletes))
	for i, t := range deletes {
		bare[i] = Tuple{User: t.User, Relation: t.Relation, Object: t.Object}
	}
	if err := send("deletes", bare); err != nil {
		return err
	}
	return send("writes", writes)
}

func (c *OpenFGA) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/stores/"+c.storeID+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return fmt.Errorf("openfga %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
