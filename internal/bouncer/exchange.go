/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A backend is told who is asking in the identity headers, and may believe
// them only as far as it believes that nothing but the front door reaches
// it. A route that says exchangeToken does not ask its backend to believe
// that: its backend is handed a token of the person, issued by the tenant's
// realm for that app alone, and verifies it like any token.
//
// The session's own token cannot be that token. It is valid at the director
// and at every sibling, which is why no backend is handed it. So it is
// exchanged (RFC 8693) for one whose audience is the app, by a client of the
// realm that exists for nothing else: it cannot sign anybody in and cannot
// ask for a token of its own, and it is admitted to the exchange only for a
// session's token that names it as an audience.

// ExchangeClientID is that client, in every tenant's realm.
const ExchangeClientID = "gentian-edge-exchange"

// Exchanger turns a session's token into one for a single app.
type Exchanger interface {
	// Exchange asks the realm for a token of the subject token's person with
	// the one scope given. It returns the token and when it runs out.
	Exchange(ctx context.Context, realm, subjectToken, scope string) (string, time.Time, error)
}

// realmNamePattern is what a realm may be called where its name becomes a
// path: of the token endpoint, and of the file its client secret is read
// from.
var realmNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// RealmExchanger exchanges at Keycloak's token endpoint.
type RealmExchanger struct {
	// TokenBase is the address the realms are reached at, up to and
	// excluding "/realms/<realm>".
	TokenBase string
	// SecretsDir holds the exchange client's secret of each realm, in a file
	// named after the realm. Read on every exchange, so a secret that was
	// replaced is the one presented.
	SecretsDir string
	Client     *http.Client
	Now        func() time.Time
}

// Exchange implements Exchanger.
func (e *RealmExchanger) Exchange(ctx context.Context, realm, subjectToken, scope string) (string, time.Time, error) {
	if !realmNamePattern.MatchString(realm) {
		return "", time.Time{}, fmt.Errorf("realm %q is not a name", realm)
	}
	if subjectToken == "" || scope == "" || strings.ContainsAny(scope, " \t") {
		return "", time.Time{}, errors.New("an exchange needs a session's token and exactly one scope")
	}
	secret, err := os.ReadFile(filepath.Join(e.SecretsDir, realm))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("no exchange client secret for realm %s: %w", realm, err)
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"client_id":            {ExchangeClientID},
		"client_secret":        {strings.TrimSpace(string(secret))},
		"subject_token":        {subjectToken},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":                {scope},
	}
	endpoint := strings.TrimRight(e.TokenBase, "/") + "/realms/" + realm + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, err
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", time.Time{}, fmt.Errorf("the realm answered %d with no JSON", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || answer.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("the realm refused the exchange (%d): %s %s", resp.StatusCode, answer.Error, answer.Description)
	}
	// The realm says which scopes it granted. One that granted another, or
	// more than the one asked, issued a token for more than this app.
	if answer.Scope != scope {
		return "", time.Time{}, fmt.Errorf("asked for scope %q and was given %q", scope, answer.Scope)
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	return answer.AccessToken, now().Add(time.Duration(answer.ExpiresIn) * time.Second), nil
}

// exchangeMargin is how long before it runs out an exchanged token stops
// being handed on: a request that carries it must still arrive with it valid.
const exchangeMargin = 30 * time.Second

// exchangeCache keeps one exchanged token per session and scope until it is
// about to run out, so that the realm is asked once in a token's lifetime
// and not once per request.
//
// Keyed by the session as well as the person: a token is the session's, and
// ends with it at the realm. The session's own token is what the Gateway
// refreshes; while that still verifies here, the exchanged one obtained under
// it is handed on until its own expiry, which the realm set to minutes.
type exchangeCache struct {
	exchanger Exchanger
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]exchanged
}

type exchanged struct {
	token   string
	expires time.Time
}

func newExchangeCache(e Exchanger, now func() time.Time) *exchangeCache {
	return &exchangeCache{exchanger: e, now: now, entries: map[string]exchanged{}}
}

func (c *exchangeCache) token(ctx context.Context, who identity, subjectToken, scope string) (string, error) {
	if c.exchanger == nil {
		return "", errors.New("no token exchange is configured")
	}
	key := who.realm + "\x00" + who.subject + "\x00" + who.session + "\x00" + scope
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Add(exchangeMargin).Before(e.expires) {
		c.mu.Unlock()
		return e.token, nil
	}
	c.mu.Unlock()

	token, expires, err := c.exchanger.Exchange(ctx, who.realm, subjectToken, scope)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Entries that ran out are dropped as new ones arrive, so the map holds
	// the sessions that are in use and not every one there ever was.
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = exchanged{token: token, expires: expires}
	return token, nil
}
