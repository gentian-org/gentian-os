/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package lifecycle is the client for the operator's app-lifecycle API. The
// director uses it for its commands and for the questions a commit depends
// on; the usher uses it, with a token of its own that only reads, to answer
// a person's reads of live state.
//
// Only ever a question. The director holds no cluster credential, and the
// operator's API is how it learns what only the cluster knows -- a tenant's
// enforced ceiling, what is committed under it, which plans exist and which
// this tenant may move to. Every answer is relayed to the caller as the
// operator gave it, and the one write in this area, choosing a plan, is a
// commit the director makes to git after validating the choice against these
// answers. The operator has no write here to call.
package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one operator.
type Client struct {
	base string
	// token is asked for on every request, so a token handed over in a file
	// that appears or changes after start is followed without a restart.
	token func() string
	// who names the caller in the one message a person may see about the
	// token itself.
	who  string
	http *http.Client
	// stream has no timeout: it carries bundle downloads, which take as long
	// as the bundle is big and are bounded by the caller's context instead.
	stream *http.Client
}

// New returns a client for the operator's API at base.
//
// token is the shared secret that API requires. The operator used to require
// nothing, which made the actor header this client sets a claim rather than a
// proof: any pod that could reach the Service could act as anybody.
func New(base, token string) *Client {
	c := NewReader(base, func() string { return token })
	c.who = "director"
	return c
}

// NewReader returns a client that asks token for its token on every request.
// It is the usher's: the token it presents admits reads only.
func NewReader(base string, token func() string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		who:    "usher",
		http:   &http.Client{Timeout: 30 * time.Second},
		stream: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}},
	}
}

// authorize presents the shared token. Called on every request, including
// reads: a tenant's installed apps and its usage are its own business.
func (c *Client) authorize(req *http.Request) {
	if token := c.token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// Get relays one read and returns the status and body as the operator
// answered them. The body is passed through rather than decoded: what a
// tenant's state or its usage history looks like is the operator's to say,
// and the console renders it as such.
func (c *Client) Get(ctx context.Context, path string, query url.Values) (int, []byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	status, body := c.refusedByOperator(resp.StatusCode, body)
	return status, body, nil
}

// Stream relays one read whose body is not decoded and may be large -- a
// bundle download. The caller owns the response and closes its body; the
// client used has no overall timeout, because the transfer takes as long as
// the bundle is big.
func (c *Client) Stream(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		status, body := c.refusedByOperator(resp.StatusCode, nil)
		resp.StatusCode = status
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Del("Content-Disposition")
		resp.Header.Del("Content-Length")
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}

// Upload relays one POST whose body is streamed through unread -- a bundle
// upload -- and answers as Do does.
func (c *Client) Upload(ctx context.Context, path, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	c.authorize(req)
	resp, err := c.stream.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	status, answer := c.refusedByOperator(resp.StatusCode, answer)
	return status, answer, nil
}

// refusedByOperator rewrites the operator refusing this client's own token.
//
// The app-lifecycle API answers 401 only to a wrong shared token: a fault
// between two platform services. Passed through unchanged it reached the
// browser as a 401, which every console reads as "your session expired" and
// answers by signing in again -- so a token mismatch made the Operations
// Console reload several times a second rather than say what was wrong. It
// leaves here as 502 Bad Gateway, which is what it is.
func (c *Client) refusedByOperator(status int, body []byte) (int, []byte) {
	if status != http.StatusUnauthorized {
		return status, body
	}
	detail := "the operator refused the " + c.who + "'s token for its app-lifecycle API; the two hold different tokens"
	answer, _ := json.Marshal(map[string]string{"detail": detail})
	return http.StatusBadGateway, answer
}

// Plan is one plan as the operator presents it for a tenant.
type Plan struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName"`
	Tier        int32             `json:"tier"`
	ProductSku  string            `json:"productSku,omitempty"`
	Quotas      map[string]string `json:"quotas"`
	Current     bool              `json:"current,omitempty"`
	Selectable  bool              `json:"selectable"`
	Blocked     string            `json:"blocked,omitempty"`
	// BlockedBy is the rule behind Blocked: self-service, entitlement or fit.
	BlockedBy string `json:"blockedBy,omitempty"`
}

// UpstreamError is an answer from the operator that was not 200.
type UpstreamError struct {
	Status  int
	Message string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("app-lifecycle API answered %d: %s", e.Status, e.Message)
}

// Plans fetches the catalogue as it applies to one tenant. selfService
// withholds the plans a tenant administrator may not pick for themselves.
func (c *Client) Plans(ctx context.Context, tenant string, selfService bool) ([]Plan, error) {
	q := url.Values{}
	if selfService {
		q.Set("selfService", "true")
	}
	status, body, err := c.Get(ctx, "/v1/tenants/"+url.PathEscape(tenant)+"/resources/plans", q)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &UpstreamError{Status: status, Message: ErrorMessage(body)}
	}
	var answer struct {
		Plans []Plan `json:"plans"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("app-lifecycle API: plans: %w", err)
	}
	return answer.Plans, nil
}

// ErrorMessage reads the operator's {"detail": "..."} body, or returns the
// body itself when it is not that shape.
func ErrorMessage(body []byte) string {
	var e struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &e) == nil && e.Detail != "" {
		return e.Detail
	}
	return strings.TrimSpace(string(body))
}

// Do asks the operator to do something once, as the person named.
//
// The only write in this client, and it is not a write of state: the director
// writes state to git. What the cluster is asked to do here happens now,
// leaves no commit, and so carries the actor on the request instead — which
// is why the header exists and why nothing but the director may set it.
func (c *Client) Do(ctx context.Context, path, actor string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	if actor != "" {
		req.Header.Set("X-Gentian-Actor", actor)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	status, answer := c.refusedByOperator(resp.StatusCode, answer)
	return status, answer, nil
}
