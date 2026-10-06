/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package lifecycle is the client for the operator's app-lifecycle API. The
// director uses it for its commands and for the questions a commit depends
// on; the usher uses it, under an identity of its own that only reads, to
// answer a person's reads of live state.
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
	"os"
	"strings"
	"time"
)

// Client talks to one operator.
type Client struct {
	base string
	// token is asked for on every request: it is a ServiceAccount token the
	// kubelet projects into a file and replaces before it expires, so the
	// one read at start would stop being valid within minutes.
	token func() string
	// who names the caller in the one message a person may see about the
	// token itself.
	who  string
	http *http.Client
	// stream has no timeout: it carries bundle downloads, which take as long
	// as the bundle is big and are bounded by the caller's context instead.
	stream *http.Client
	// patient has no timeout of its own at all, not even for the response
	// to begin: it carries the one action whose answer is the end of minutes
	// of work, and is bounded by the deadline Patient puts on the context.
	patient *http.Client
}

// PurgeDeadline is how long the director waits for the operator to answer a
// purge of an app.
//
// A purge is one request answered when it is over, and the operator gives
// itself four and a half minutes for it (applifecycle's purgeBudget): it
// drops databases, runs deletion Jobs and waits for volumes to go. The
// ordinary thirty seconds of this client cut such a request off while the
// operator was still destroying things, and the person was told the operator
// had not answered. This is longer than the operator's budget, so the
// operator always answers first -- finished, or stopped and saying where --
// and shorter than the director's own write deadline, so the director still
// has time to pass the answer on.
const PurgeDeadline = 5 * time.Minute

type patientKey struct{}

// Patient returns a context under which Do waits up to d for its answer
// instead of this client's ordinary timeout. For an action known to take
// long; everything else keeps the short timeout, which is what stops a
// relay from hanging on an operator that is not there.
func Patient(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithValue(ctx, patientKey{}, true), d)
}

// New returns a client for the operator's API at base, for the director.
//
// token returns the caller's own ServiceAccount token, issued for the
// operator's audience; the operator asks the API server whose it is and
// admits the director's ServiceAccount to every route. That is what makes
// the actor header this client sets worth recording: no other identity is
// admitted to a route that reads it.
func New(base string, token func() string) *Client {
	c := NewReader(base, token)
	c.who = "director"
	return c
}

// TokenFile returns a token source that reads path on every call. A
// projected ServiceAccount token is rewritten in place as it nears expiry,
// so it is read when it is needed and never remembered. A file that is
// missing or unreadable yields no token, and the operator refuses.
func TokenFile(path string) func() string {
	return func() string {
		token, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(token))
	}
}

// NewReader returns a client for the usher: the identity its token carries
// is admitted to reads only.
func NewReader(base string, token func() string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		who:    "usher",
		http:   &http.Client{Timeout: 30 * time.Second},
		stream: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}},
		// A transport of its own, so that nothing set on the default one is
		// inherited by the request that must be allowed to take minutes.
		patient: &http.Client{Transport: &http.Transport{}},
	}
}

// authorize presents the caller's token. Called on every request, including
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
// The app-lifecycle API answers 401 only when this client's own
// ServiceAccount token is missing or not valid for it: a fault between two
// platform services. Passed through unchanged it reached the browser as a
// 401, which every console reads as "your session expired" and answers by
// signing in again -- so a refused token made the Operations Console reload
// several times a second rather than say what was wrong. It leaves here as
// 502 Bad Gateway, which is what it is.
func (c *Client) refusedByOperator(status int, body []byte) (int, []byte) {
	if status != http.StatusUnauthorized {
		return status, body
	}
	detail := "the operator refused the " + c.who + "'s token for its app-lifecycle API; it presented none, or one the API server does not vouch for"
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
	client := c.http
	if patient, _ := ctx.Value(patientKey{}).(bool); patient {
		client = c.patient
	}
	resp, err := client.Do(req)
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
