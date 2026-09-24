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

// Package lifecycle is the director's client for the operator's app-lifecycle
// API: the one place the director asks the cluster a question.
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
	http *http.Client
}

// New returns a client for the operator's API at base.
func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 30 * time.Second}}
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
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("app-lifecycle API: %w", err)
	}
	return resp.StatusCode, body, nil
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
