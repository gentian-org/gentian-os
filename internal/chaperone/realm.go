/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The tenant's realm, for a person who is present. Two questions, both the
// realm's to answer: is this token one it issued to that person and made out
// to this chaperone; and what is that person's token for a single app. The
// chaperone asks the second as the client the platform gave it (the Secret
// vouching-<profile>), and the realm exchanges only a token that names that
// client.

const (
	grantExchange   = "urn:ietf:params:oauth:grant-type:token-exchange"
	typeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
)

// CertsURL is where the realm publishes its keys, from where it issues
// tokens.
func CertsURL(tokenURL string) string {
	if i := strings.LastIndex(tokenURL, "/"); i >= 0 {
		return tokenURL[:i] + "/certs"
	}
	return tokenURL + "/certs"
}

// RealmPersons verifies a present person's access token.
type RealmPersons struct {
	keys     *keySet
	issuer   string
	audience string
}

// NewRealmPersons verifies tokens of issuer made out to audience against the
// realm's keys at certsURL.
func NewRealmPersons(issuer, audience, certsURL string, client *http.Client) *RealmPersons {
	return &RealmPersons{keys: newKeySet(certsURL, client), issuer: issuer, audience: audience}
}

func (r *RealmPersons) Verify(ctx context.Context, token string, now int64) (Present, error) {
	var std jwt.Claims
	var own struct {
		Type   string `json:"typ"`
		Client string `json:"azp"`
	}
	algorithms := []jose.SignatureAlgorithm{jose.RS256, jose.ES256}
	if err := r.keys.verified(ctx, token, algorithms, &std, &own); err != nil {
		return Present{}, fmt.Errorf("the token is not good: %w", err)
	}
	if err := standard(std, r.issuer, r.audience, now); err != nil {
		return Present{}, fmt.Errorf("the token is not good: %w", err)
	}
	if own.Type != "Bearer" {
		return Present{}, errors.New("this is not an access token")
	}
	if own.Client == "" {
		return Present{}, errors.New("the token names no program")
	}
	return Present{Person: std.Subject, Client: own.Client, Token: token}, nil
}

// RealmCredentials exchanges a present person's token for one valid at a
// single app.
type RealmCredentials struct {
	tokenURL string
	id       string
	secret   string
	client   *http.Client
}

// NewRealmCredentials asks the realm at tokenURL as the client id.
func NewRealmCredentials(tokenURL, id, secret string, client *http.Client) *RealmCredentials {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &RealmCredentials{tokenURL: tokenURL, id: id, secret: secret, client: client}
}

func (r *RealmCredentials) ForApp(ctx context.Context, token, app string) (string, error) {
	form := url.Values{
		"grant_type":           {grantExchange},
		"subject_token":        {token},
		"subject_token_type":   {typeAccessToken},
		"requested_token_type": {typeAccessToken},
		"scope":                {"app-" + app},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", adapterError("the realm could not be asked: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(r.id, r.secret)
	resp, err := r.client.Do(req)
	if err != nil {
		return "", adapterError("the realm could not be asked: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		why := body.ErrorDescription
		if why == "" {
			why = body.Error
		}
		if why == "" {
			why = fmt.Sprint(resp.StatusCode)
		}
		return "", adapterError("the realm gave no token for %s: %s", app, why)
	}
	return body.AccessToken, nil
}
