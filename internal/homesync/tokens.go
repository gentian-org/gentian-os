/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The service vouches for people (requires.services.vouching): it signs a
// statement of a few minutes that names a person, and the tenant's realm
// answers with a token of that person for the one app the scope names. The
// realm reads the keys the statements are signed with from the service
// (Keys.JWKS), and answers only for a person linked to the service.

// Keys are the service's signing keys: one, made when the process starts. A
// statement names it by its id, and the realm fetches the set again when it
// meets an id it does not know, so a restart costs one fetch.
type Keys struct {
	private *rsa.PrivateKey
	id      string
}

// NewKeys makes the process's key.
func NewKeys() (*Keys, error) {
	k, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Keys{private: k, id: hex.EncodeToString(b)}, nil
}

// JWKS is the public half, as the realm reads it.
func (k *Keys) JWKS() ([]byte, error) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &k.private.PublicKey, KeyID: k.id, Algorithm: string(jose.RS256), Use: "sig",
	}}}
	return json.Marshal(set)
}

// Vouching is what the platform tells the service about its client in the
// tenant's realm (the Secret vouching-<profile>).
type Vouching struct {
	Issuer       string // VOUCHING_ISSUER: what the statements name as issuer
	ClientID     string // VOUCHING_CLIENT_ID
	ClientSecret string // VOUCHING_CLIENT_SECRET
	TokenURL     string // VOUCHING_TOKEN_URL
}

// realmAddress is where the realm answers the service: the token address
// without the path Keycloak puts after the realm.
func (v Vouching) realmAddress() string {
	return strings.TrimSuffix(v.TokenURL, "/protocol/openid-connect/token")
}

// ErrRefused is the realm's refusal of a statement: the person is not
// linked to the service, the scope is not the service's client's, or the
// statement is not what the realm takes. Its reason follows it.
var ErrRefused = errors.New("the realm refused a token of this person for this scope")

// Tokens obtains a person's token for one scope, and keeps it until shortly
// before it runs out.
type Tokens struct {
	Keys     *Keys
	Vouching Vouching
	Client   *http.Client
	Now      func() time.Time

	mu    sync.Mutex
	cache map[string]cached
	// issuer is the realm's issuer, which a statement names as its
	// audience; read once from the realm's discovery document.
	issuer string
}

type cached struct {
	token   string
	expires time.Time
}

// statementLife is how long a statement is good for: long enough for one
// request, and no longer.
const statementLife = 2 * time.Minute

// tokenMargin is how long before it runs out a token is asked for again.
const tokenMargin = 30 * time.Second

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// For is the person's token for the scope.
func (t *Tokens) For(ctx context.Context, person, scope string) (string, error) {
	key := person + "\n" + scope
	t.mu.Lock()
	if c, ok := t.cache[key]; ok && t.now().Add(tokenMargin).Before(c.expires) {
		t.mu.Unlock()
		return c.token, nil
	}
	t.mu.Unlock()

	issuer, err := t.realmIssuer(ctx)
	if err != nil {
		return "", err
	}
	statement, err := t.statement(person, issuer)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":     {statement},
		"client_id":     {t.Vouching.ClientID},
		"client_secret": {t.Vouching.ClientSecret},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Vouching.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the realm could not be asked: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&answer)
	switch {
	case resp.StatusCode == http.StatusOK && answer.AccessToken != "":
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return "", fmt.Errorf("%w: %s %s", ErrRefused, answer.Error, answer.Description)
	default:
		return "", fmt.Errorf("the realm answered %d", resp.StatusCode)
	}
	t.mu.Lock()
	if t.cache == nil {
		t.cache = map[string]cached{}
	}
	t.cache[key] = cached{token: answer.AccessToken, expires: t.now().Add(time.Duration(answer.ExpiresIn) * time.Second)}
	t.mu.Unlock()
	return answer.AccessToken, nil
}

// realmIssuer is the realm's issuer as its discovery document names it. The
// realm answers the service at an address inside the cluster, but names
// itself by the address a browser reaches it at (its frontendUrl), and a
// statement must name that.
func (t *Tokens) realmIssuer(ctx context.Context) (string, error) {
	t.mu.Lock()
	issuer := t.issuer
	t.mu.Unlock()
	if issuer != "" {
		return issuer, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Vouching.realmAddress()+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the realm's discovery document could not be read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the realm's discovery document answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || doc.Issuer == "" {
		return "", fmt.Errorf("the realm's discovery document names no issuer")
	}
	t.mu.Lock()
	t.issuer = doc.Issuer
	t.mu.Unlock()
	return doc.Issuer, nil
}

// statement is a signed statement naming the person, for the realm.
func (t *Tokens) statement(person, audience string) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: t.Keys.private, KeyID: t.Keys.id}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	now := t.now()
	claims := jwt.Claims{
		Issuer:   t.Vouching.Issuer,
		Subject:  person,
		Audience: jwt.Audience{audience},
		ID:       hex.EncodeToString(id),
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(statementLife)),
	}
	return jwt.Signed(signer).Claims(claims).Serialize()
}
