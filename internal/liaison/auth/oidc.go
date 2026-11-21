/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package auth verifies a person's token: signature, issuer, audience.
package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Unauthorized says the token does not name a person. The caller answers 401.
type Unauthorized struct{ Detail string }

func (e *Unauthorized) Error() string { return e.Detail }

// Unavailable says the identity provider could not be asked. The caller
// answers 503.
type Unavailable struct{ Detail string }

func (e *Unavailable) Error() string { return e.Detail }

func invalid(format string, args ...any) error {
	return &Unauthorized{fmt.Sprintf(format, args...)}
}

const jwksTTL = 600 * time.Second

// OIDC verifies tokens of one identity provider for one audience.
type OIDC struct {
	issuer   string
	audience string
	client   *http.Client

	mu      sync.Mutex
	fetched time.Time
	keys    []json.RawMessage
}

// NewOIDC returns the verifier for issuer's tokens made out to audience. A
// nil client is one that waits ten seconds and follows no redirect.
func NewOIDC(issuer, audience string, client *http.Client) *OIDC {
	if client == nil {
		client = &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &OIDC{issuer: strings.TrimRight(issuer, "/"), audience: audience, client: client}
}

// jwks returns the provider's keys, asked for again after ten minutes or on
// refresh.
func (o *OIDC) jwks(ctx context.Context, refresh bool) ([]json.RawMessage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.keys != nil && !refresh && time.Since(o.fetched) < jwksTTL {
		return o.keys, nil
	}
	unavailable := func(err error) error {
		return &Unavailable{fmt.Sprintf("The identity provider's signing keys are unavailable: %v", err)}
	}
	url := o.issuer + "/protocol/openid-connect/certs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, unavailable(err)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, unavailable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, unavailable(fmt.Errorf("%s answered %d", url, resp.StatusCode))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, unavailable(err)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("the identity provider's signing keys are not a key set: %w", err)
	}
	if set.Keys == nil {
		set.Keys = []json.RawMessage{}
	}
	o.keys, o.fetched = set.Keys, time.Now()
	return o.keys, nil
}

// signingKey finds the key a token names. An unknown key is asked for once
// more, in case the provider has changed its keys.
func (o *OIDC) signingKey(ctx context.Context, kid *string) (*rsa.PublicKey, error) {
	for _, refresh := range []bool{false, true} {
		keys, err := o.jwks(ctx, refresh)
		if err != nil {
			return nil, err
		}
		for _, raw := range keys {
			var named struct {
				Kid *string `json:"kid"`
			}
			if json.Unmarshal(raw, &named) != nil {
				continue
			}
			if (named.Kid == nil) != (kid == nil) || (kid != nil && *named.Kid != *kid) {
				continue
			}
			var key jose.JSONWebKey
			if err := key.UnmarshalJSON(raw); err != nil {
				return nil, invalid("Could not read the signing key: %v", err)
			}
			public, ok := key.Key.(*rsa.PublicKey)
			if !ok {
				return nil, invalid("Not a public RSA key")
			}
			return public, nil
		}
	}
	return nil, invalid("Unknown signing key")
}

func segment(part, what string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(part, "="))
	if err != nil {
		return invalid("Invalid %s padding", what)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return invalid("Invalid %s string: %v", what, err)
	}
	return nil
}

// Verify returns the person a token names.
func (o *OIDC) Verify(ctx context.Context, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", invalid("Not enough segments")
	}
	var header struct {
		Alg string  `json:"alg"`
		Kid *string `json:"kid"`
	}
	if err := segment(parts[0], "header", &header); err != nil {
		return "", err
	}
	key, err := o.signingKey(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	if header.Alg != "RS256" {
		return "", invalid("The specified alg value is not allowed")
	}
	signature, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[2], "="))
	if err != nil {
		return "", invalid("Invalid crypto padding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return "", invalid("Signature verification failed")
	}
	var claims map[string]any
	if err := segment(parts[1], "payload", &claims); err != nil {
		return "", err
	}
	if claims == nil {
		return "", invalid("Invalid payload string: must be a json object")
	}
	return o.subject(claims, float64(time.Now().UnixNano())/1e9)
}

// subject checks the claims and returns the person.
func (o *OIDC) subject(claims map[string]any, now float64) (string, error) {
	for _, required := range []string{"exp", "sub"} {
		if claims[required] == nil {
			return "", invalid("Token is missing the %q claim", required)
		}
	}
	whole := func(name, title string) (float64, bool, error) {
		value, present := claims[name]
		if !present {
			return 0, false, nil
		}
		number, ok := value.(float64)
		if !ok {
			return 0, false, invalid("%s claim (%s) must be an integer.", title, name)
		}
		return math.Trunc(number), true, nil
	}
	if iat, present, err := whole("iat", "Issued At"); err != nil {
		return "", err
	} else if present && iat > now {
		return "", invalid("The token is not yet valid (iat)")
	}
	if nbf, present, err := whole("nbf", "Not Before"); err != nil {
		return "", err
	} else if present && nbf > now {
		return "", invalid("The token is not yet valid (nbf)")
	}
	if exp, _, err := whole("exp", "Expiration Time"); err != nil {
		return "", err
	} else if exp <= now {
		return "", invalid("Signature has expired")
	}

	if claims["iss"] == nil {
		return "", invalid("Token is missing the %q claim", "iss")
	}
	if claims["iss"] != o.issuer {
		return "", invalid("Invalid issuer")
	}

	if claims["aud"] == nil {
		return "", invalid("Token is missing the %q claim", "aud")
	}
	audiences, ok := claims["aud"].([]any)
	if single, isString := claims["aud"].(string); isString {
		audiences, ok = []any{single}, true
	}
	if !ok {
		return "", invalid("Invalid claim format in token")
	}
	matched := false
	for _, audience := range audiences {
		named, isString := audience.(string)
		if !isString {
			return "", invalid("Invalid claim format in token")
		}
		matched = matched || named == o.audience
	}
	if !matched {
		return "", invalid("Audience doesn't match")
	}

	subject, ok := claims["sub"].(string)
	if !ok {
		return "", invalid("Subject must be a string")
	}
	if jti, present := claims["jti"]; present {
		if _, ok := jti.(string); !ok {
			return "", invalid("JWT ID must be a string")
		}
	}
	return subject, nil
}
