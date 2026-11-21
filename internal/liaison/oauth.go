/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package liaison

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A person signs in at an outside service by OAuth 2.0 authorization code
// with PKCE (RFC 7636). The service's authorization server is found by its
// metadata (RFC 8414), and the liaison registers itself there as a client
// when a person connects (RFC 7591): a public client, which holds no secret
// of its own unless the server hands it one.

// Authority is what an authorization server says of itself, as far as the
// liaison asks.
type Authority struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	Registration  string `json:"registration_endpoint"`
	Revocation    string `json:"revocation_endpoint"`
}

// Client is the liaison as an authorization server knows it.
type Client struct {
	ID     string `json:"client_id"`
	Secret string `json:"client_secret,omitempty"`
}

// Grant is what a token endpoint answers.
type Grant struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// ErrGrantRefused is a token endpoint saying no to what it was shown: the
// person has to sign in again.
var ErrGrantRefused = errors.New("the service refused the credential")

// OAuth speaks to authorization servers.
type OAuth struct {
	// HTTP follows no redirect: an endpoint answers where it was named.
	HTTP *http.Client
}

// NewOAuth returns an OAuth with a client that waits fifteen seconds.
func NewOAuth(client *http.Client) *OAuth {
	if client == nil {
		client = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &OAuth{HTTP: client}
}

// secure reports whether address is an https address with a host and
// nothing that hides one.
func secure(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// Discover reads the metadata of the authorization server at issuer. Every
// endpoint it names has to be https, and the two the sign-in needs have to
// be there.
func (o *OAuth) Discover(ctx context.Context, issuer string) (Authority, error) {
	var server Authority
	if !secure(issuer) {
		return server, fmt.Errorf("the authorization server %q is not an https address", issuer)
	}
	u, _ := url.Parse(issuer)
	// RFC 8414 §3: the well-known segment goes between the host and the
	// issuer's path.
	address := u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server" + strings.TrimRight(u.Path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return server, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return server, fmt.Errorf("the authorization server could not be asked: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return server, fmt.Errorf("the authorization server answered %d for its metadata", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&server); err != nil {
		return server, fmt.Errorf("the authorization server's metadata could not be read: %w", err)
	}
	if !secure(server.Authorization) || !secure(server.Token) {
		return server, errors.New("the authorization server names no https endpoints for sign-in and tokens")
	}
	for _, optional := range []*string{&server.Registration, &server.Revocation} {
		if *optional != "" && !secure(*optional) {
			*optional = ""
		}
	}
	return server, nil
}

// Register makes the liaison a client of server, with the one address a
// sign-in comes back to.
func (o *OAuth) Register(ctx context.Context, server Authority, name, redirect string) (Client, error) {
	var client Client
	if server.Registration == "" {
		return client, errors.New("the authorization server registers no clients, and none is configured for it")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                name,
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.Registration, strings.NewReader(string(body)))
	if err != nil {
		return client, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return client, fmt.Errorf("the authorization server could not be asked: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return client, fmt.Errorf("the authorization server answered %d to the registration", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&client); err != nil || client.ID == "" {
		return client, errors.New("the authorization server's registration could not be read")
	}
	return client, nil
}

// Verifier is a fresh PKCE code verifier, and Challenge what is sent of it.
func Verifier() string { return random(32) }

// Challenge is the S256 challenge of a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func random(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// AuthorizeURL is where the person is sent to sign in. resource is the tool
// server the credential is for (RFC 8707).
func AuthorizeURL(server Authority, client Client, redirect, state, verifier, resource string, scopes []string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {client.ID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {Challenge(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {resource},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	separator := "?"
	if strings.Contains(server.Authorization, "?") {
		separator = "&"
	}
	return server.Authorization + separator + q.Encode()
}

// token posts form to the token endpoint as client.
func (o *OAuth) token(ctx context.Context, endpoint string, client Client, form url.Values) (Grant, error) {
	var grant Grant
	if !secure(endpoint) {
		return grant, errors.New("the token endpoint is not an https address")
	}
	form.Set("client_id", client.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return grant, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if client.Secret != "" {
		req.SetBasicAuth(url.QueryEscape(client.ID), url.QueryEscape(client.Secret))
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return grant, fmt.Errorf("the service could not be asked for a token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return grant, ErrGrantRefused
	}
	if resp.StatusCode != http.StatusOK {
		return grant, fmt.Errorf("the service answered %d for a token", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&grant); err != nil || grant.AccessToken == "" {
		return grant, errors.New("the service's token answer could not be read")
	}
	return grant, nil
}

// Exchange turns the code a sign-in came back with into a grant.
func (o *OAuth) Exchange(ctx context.Context, endpoint string, client Client, code, verifier, redirect, resource string) (Grant, error) {
	return o.token(ctx, endpoint, client, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
		"resource":      {resource},
	})
}

// Refresh asks for a new access token with the lasting one.
func (o *OAuth) Refresh(ctx context.Context, endpoint string, client Client, refresh, resource string) (Grant, error) {
	return o.token(ctx, endpoint, client, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"resource":      {resource},
	})
}

// Revoke tells the service the lasting credential is given up (RFC 7009).
// A service that names no endpoint for it is not told.
func (o *OAuth) Revoke(ctx context.Context, endpoint string, client Client, token string) error {
	if endpoint == "" || token == "" {
		return nil
	}
	if !secure(endpoint) {
		return errors.New("the revocation endpoint is not an https address")
	}
	form := url.Values{"token": {token}, "client_id": {client.ID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if client.Secret != "" {
		req.SetBasicAuth(url.QueryEscape(client.ID), url.QueryEscape(client.Secret))
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the service answered %d to the revocation", resp.StatusCode)
	}
	return nil
}
