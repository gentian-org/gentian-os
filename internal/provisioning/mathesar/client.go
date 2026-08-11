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

// Package mathesar speaks Mathesar's own /api/rpc/v0/ JSON-RPC 2.0 endpoint.
// It backs the "mathesar-rpc" PrivilegedRoleSpec protocol (see
// api/v1alpha1.PrivilegedRoleSpec) — the operator has no other way to grant
// or revoke Mathesar's is_superuser flag, since upstream Mathesar never
// derives it from an OIDC claim (mathesar/sso.py always creates new SSO
// logins as regular, non-superuser accounts).
package mathesar

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultHTTPTimeout = 15 * time.Second

// User is Mathesar's own account record, as returned by its users.* RPC
// methods (see mathesar/rpc/users.py UserInfo upstream).
type User struct {
	ID              int    `json:"id"`
	Username        string `json:"username"`
	IsSuperuser     bool   `json:"is_superuser"`
	Email           string `json:"email"`
	FullName        string `json:"full_name"`
	DisplayLanguage string `json:"display_language"`
}

// Client authenticates to Mathesar's JSON-RPC endpoint as an existing
// superuser via HTTP Basic Auth — the only auth mode users.add/users.list/
// users.patch_other accept (auth="superuser" in mathesar/rpc/users.py
// upstream, enforced by django-modernrpc's http_basic_auth_superuser_required
// decorator). There is no bootstrap path through this client: the very first
// superuser must already exist (see the profile's spec.postInstallJob).
type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

// NewClient builds a client against baseURL (Mathesar's own origin, e.g.
// "http://mathesar-ce.tenant-demo.svc.cluster.local:8000" — an in-cluster
// service address, never the public tenant hostname, per the server-to-server
// rule in docs/app-profile-guide.md §2) using an existing superuser's
// credentials.
func NewClient(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/") + "/api/rpc/v0/",
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
	ID      int    `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// call issues one JSON-RPC 2.0 request. params is marshalled as a JSON
// object (named parameters) — Mathesar's RPC methods take keyword-only
// arguments, so params keys must match those argument names exactly.
func (c *Client) call(ctx context.Context, method string, params map[string]any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: 1})
	if err != nil {
		return fmt.Errorf("mathesar rpc %s: encode request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mathesar rpc %s: %w", method, err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mathesar rpc %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("mathesar rpc %s: read response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mathesar rpc %s: status %d: %s", method, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rpcResp rpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return fmt.Errorf("mathesar rpc %s: decode response: %w", method, err)
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("mathesar rpc %s: %d %s", method, rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rpcResp.Result, out)
}

// ListUsers returns every Mathesar account (users.list).
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var out []User
	if err := c.call(ctx, "users.list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddUser creates a new Mathesar account with a random throwaway password.
// The account is meant to be claimed by SSO — Mathesar links an OIDC login to
// an existing local account matched by email rather than creating one itself
// when an admin has already provisioned it (see mathesar/sso.py's save_user
// upstream) — so this password is never meant to be used to log in directly.
func (c *Client) AddUser(ctx context.Context, username, email string, isSuperuser bool) (User, error) {
	password, err := randomPassword()
	if err != nil {
		return User{}, fmt.Errorf("generate throwaway password: %w", err)
	}
	params := map[string]any{
		"user_def": map[string]any{
			"username":     username,
			"password":     password,
			"is_superuser": isSuperuser,
			"email":        email,
		},
	}
	var out User
	if err := c.call(ctx, "users.add", params, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

// SetSuperuser flips is_superuser on an existing account. users.patch_other
// replaces the whole record server-side, so every field is resent even
// though only is_superuser changes — otherwise the call would blank out the
// user's username/email/full_name/display_language.
func (c *Client) SetSuperuser(ctx context.Context, user User, isSuperuser bool) (User, error) {
	params := map[string]any{
		"user_id":          user.ID,
		"username":         user.Username,
		"is_superuser":     isSuperuser,
		"email":            user.Email,
		"full_name":        user.FullName,
		"display_language": user.DisplayLanguage,
	}
	var out User
	if err := c.call(ctx, "users.patch_other", params, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

func randomPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
