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

// Package identity is how the director speaks for Keycloak.
//
// Keycloak holds WHO, the way git holds HOW and OpenFGA holds WHAT somebody
// may do, and a director write into any of the three is admissible on the
// same three conditions: it is authenticated, it is evaluated, and it is
// recorded. People cannot be derived from the state of a cluster and cannot
// go into git, so managing them is the director's to do as an action rather
// than as a commit.
//
// Two rules hold this package together, and both are about what it must NOT
// let happen.
//
// The credential is the director's; the authority is always the caller's.
// Nothing here checks anything: every call arrives having already been
// authorised against OpenFGA with the caller's own token, and this package's
// job is to make it impossible for that check to have been made about the
// wrong realm. So no method takes a realm from a request. A realm arrives
// only through Realm(), which the caller builds from the tenant it just
// checked, and a Realm can only be used with a credential issued for it.
//
// One credential per realm, not one credential for every realm. The plan
// named Keycloak 26.2's fine-grained admin permissions for this, and they
// cannot express it: those permissions scope a client WITHIN its own realm,
// and a client in the kernel realm has no authority in a tenant realm at all
// regardless of them. Cross-realm administration means a client in `master`,
// which is the "one realm-admin for every realm" the plan rejected for the
// right reason -- a single missing check would then be a cross-tenant breach.
// A credential that exists per realm makes that structural instead of
// policed: to touch another tenant's realm the director would have to be
// handed a credential it was never given.
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNoCredential means this director holds nothing for that realm. It is not
// a failure of the caller's authority -- they may well hold the relation --
// but of this deployment's provisioning, and the two read differently to
// whoever has to fix it.
var ErrNoCredential = errors.New("no Keycloak credential for this realm")

// ErrNotFound is a Keycloak 404: the user, the group or the realm is not
// there. Distinguished so a handler can answer 404 rather than 502.
var ErrNotFound = errors.New("not found in the realm")

// ErrConflict is a Keycloak 409, which for a user creation means the address
// or the username is already taken.
var ErrConflict = errors.New("already exists in the realm")

// Realm is a realm this director has been given a credential for.
//
// Unexported field and no literal: the only way to obtain one is through a
// Client, which refuses a realm it holds nothing for. A handler therefore
// cannot spell a realm name into an admin call, which is the single mistake
// that would turn a missing authorization check into a cross-tenant one.
type Realm struct {
	name string
	// groupPrefix confines this token to one group subtree; empty means the
	// whole realm. Set through Scoped, for the tenant that shares a realm.
	groupPrefix string
}

// Name is the realm as Keycloak names it. For logging and for the record --
// never for building a URL outside this package.
func (r Realm) Name() string { return r.name }

// Credential is one realm's service account.
//
// TokenRealm is where the client itself lives and is where the token is
// minted; Realm is what it may administer. They are the same realm in the
// per-realm design this package expects, and are kept apart so a deployment
// that has to mint in `master` is expressible without rewriting the client.
type Credential struct {
	Realm        string
	TokenRealm   string
	ClientID     string
	ClientSecret string
}

// CredentialSource hands out the credential for one realm.
//
// An interface because where these come from is a provisioning decision and
// not this package's: a mounted directory, a Secret projected by ESO, or a
// test's map. What the source must guarantee is the only thing this package
// relies on: For(r) never answers with a credential that can administer a
// realm other than r.
type CredentialSource interface {
	For(realm string) (Credential, bool)
	// Realms lists what this director can speak for, for a readiness check
	// that can say "this cluster has no credential for tenant X" before
	// somebody discovers it in a 503.
	Realms() []string
}

// StaticSource is a CredentialSource over a fixed set, keyed by realm. It is
// what a deployment with credentials mounted at start-up uses, and what tests
// use.
type StaticSource map[string]Credential

func (s StaticSource) For(realm string) (Credential, bool) {
	c, ok := s[realm]
	return c, ok
}

func (s StaticSource) Realms() []string {
	out := make([]string, 0, len(s))
	for r := range s {
		out = append(out, r)
	}
	return out
}

// Config is what a Client needs.
type Config struct {
	// BaseURL is Keycloak's root, without /admin or /realms.
	BaseURL string
	Source  CredentialSource
	Logger  *slog.Logger
	// HTTPClient is optional; the default has a timeout because an admin
	// call on a request path must not outlive the request.
	HTTPClient *http.Client
}

// Client is the director's Keycloak administrative client.
type Client struct {
	base   string
	source CredentialSource
	http   *http.Client
	log    *slog.Logger

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	value   string
	expires time.Time
}

// New builds a Client. A director with no credentials at all is not an error
// here: the cluster may have no realm it administers yet, and the routes that
// need one refuse individually with ErrNoCredential rather than the whole
// service refusing to start.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		return nil, errors.New("identity: BaseURL is required")
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("identity: BaseURL: %w", err)
	}
	if cfg.Source == nil {
		cfg.Source = StaticSource{}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{base: base, source: cfg.Source, http: hc, log: log, tokens: map[string]cachedToken{}}, nil
}

// Realm returns the realm token for a realm this director holds a credential
// for. Every operation starts here, which is what makes "the director cannot
// reach a realm it was not given" a property of the type rather than of every
// handler remembering to check.
func (c *Client) Realm(name string) (Realm, error) {
	if name == "" || strings.ContainsAny(name, "/?#") {
		return Realm{}, fmt.Errorf("%w: %q", ErrNoCredential, name)
	}
	if _, ok := c.source.For(name); !ok {
		return Realm{}, fmt.Errorf("%w: %q", ErrNoCredential, name)
	}
	return Realm{name: name}, nil
}

// Realms is what this director can speak for.
func (c *Client) Realms() []string { return c.source.Realms() }

// token returns a service-account access token for one realm, minting a new
// one when the cached one is close to expiry.
//
// Cached per realm and not globally: two realms are two credentials and two
// tokens, and one cache entry for "the token" would hand realm A's call realm
// B's authority the moment both are in use.
func (c *Client) token(ctx context.Context, r Realm) (string, error) {
	cred, ok := c.source.For(r.name)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoCredential, r.name)
	}
	c.mu.Lock()
	if t, ok := c.tokens[r.name]; ok && time.Now().Before(t.expires) {
		c.mu.Unlock()
		return t.value, nil
	}
	c.mu.Unlock()

	tokenRealm := cred.TokenRealm
	if tokenRealm == "" {
		tokenRealm = cred.Realm
	}
	if tokenRealm == "" {
		tokenRealm = r.name
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cred.ClientID},
		"client_secret": {cred.ClientSecret},
	}
	endpoint := c.base + "/realms/" + url.PathEscape(tokenRealm) + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak token for realm %s: %w", r.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The secret is in the request, never in the answer, so the body is
		// safe to report -- and it is the only thing that says whether the
		// client is unknown, disabled or not a service account.
		return "", fmt.Errorf("keycloak token for realm %s: HTTP %d: %s",
			r.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("keycloak token for realm %s: unreadable answer", r.name)
	}
	// Thirty seconds of headroom: a token that expires between the check and
	// the call fails the call, and the realm's default lifetime is minutes.
	ttl := time.Duration(out.ExpiresIn)*time.Second - 30*time.Second
	if ttl < 5*time.Second {
		ttl = 5 * time.Second
	}
	c.mu.Lock()
	c.tokens[r.name] = cachedToken{value: out.AccessToken, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
	return out.AccessToken, nil
}

// do makes one admin call against a realm.
//
// path is appended to /admin/realms/<realm>, so every call in this package is
// scoped to the realm its credential was issued for by construction. It never
// takes a full URL.
func (c *Client) do(ctx context.Context, r Realm, method, path string, query url.Values, body any) (*http.Response, error) {
	tok, err := c.token(ctx, r)
	if err != nil {
		return nil, err
	}
	endpoint := c.base + "/admin/realms/" + url.PathEscape(r.name) + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The half of the record Keycloak cannot know.
	//
	// Keycloak writes an admin event for every one of these calls, with what
	// changed and with retention, and that is the better record of the change
	// because it is written whether the change came through here or through
	// Keycloak's own console. What it cannot see is WHO was allowed to ask:
	// authDetails names this director's service account and nothing else.
	//
	// So the request id travels with the call. The platform's event listener
	// runs inside this request's transaction and reads it back off the
	// headers, which joins Keycloak's record of the change to the director's
	// record of the authority without either one storing the other's half.
	if id := RequestIDFrom(ctx); id != "" {
		req.Header.Set(RequestIDHeader, id)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("keycloak %s %s: %w", method, path, err)
	}
	return resp, nil
}

// call makes one admin call and decodes the answer into out, which may be nil
// for a request with no body worth reading.
func (c *Client) call(ctx context.Context, r Realm, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, r, method, path, query, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err := statusError(resp.StatusCode, method, path, raw); err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// statusError turns a Keycloak status into the error the handler above needs
// to answer with. A 404 and a 409 are the caller's to hear about; everything
// else is this platform's.
func statusError(status int, method, path string, body []byte) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s", ErrNotFound, method, path)
	case status == http.StatusConflict:
		return fmt.Errorf("%w: %s %s", ErrConflict, method, path)
	}
	// Keycloak answers errors as {"error":"...","errorMessage":"..."}; the
	// message is written for an administrator and is worth passing on.
	var e struct {
		Error        string `json:"error"`
		ErrorMessage string `json:"errorMessage"`
	}
	_ = json.Unmarshal(body, &e)
	detail := e.ErrorMessage
	if detail == "" {
		detail = e.Error
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	return fmt.Errorf("keycloak %s %s: HTTP %d: %s", method, path, status, detail)
}

// ClientID is the client the director authenticates as, in every realm it
// speaks for. One name everywhere, so an operator reading a realm can tell
// what it is at a glance and an audit can find it without a convention to
// remember.
//
// Canonical here and referenced by the operator that provisions it: the two
// must agree or the operator writes a credential under a name the director
// never asks for, which reads as "this tenant has no identity".
const ClientID = "gentian-director-admin"

// RequestIDHeader is how the caller's request id reaches Keycloak's admin
// event, and through it the authority record. Named here rather than in the
// caller because the listener that reads it back has to agree on the spelling.
const RequestIDHeader = "X-Gentian-Request-Id"

// requestIDKey is the context key the director puts the request id under.
type requestIDKey struct{}

// WithRequestID carries a request id into the admin calls made under ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom reads it back. Empty when there is none, which is what a call
// made outside a request looks like -- a reconcile, or a test.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
