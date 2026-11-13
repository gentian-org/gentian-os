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
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	keysTTL = 10 * time.Minute
	// keysMinInterval is how soon the keys may be asked for again because
	// of a key that is not known: a stream of tokens naming unknown keys is
	// not a stream of fetches.
	keysMinInterval = 10 * time.Second
)

// keySet is the signing keys a party publishes, read from where it publishes
// them and kept for ten minutes. A key that is not cached is fetched once
// more before a token is refused, so a rotated key costs one fetch and not
// an outage.
type keySet struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	keys    map[string]jose.JSONWebKey
	fetched time.Time
}

func newKeySet(url string, client *http.Client) *keySet {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &keySet{url: url, client: client}
}

func (k *keySet) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the answer was %d", resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return err
	}
	keys := map[string]jose.JSONWebKey{}
	for _, key := range set.Keys {
		if key.Use == "" || key.Use == "sig" {
			keys[key.KeyID] = key
		}
	}
	k.keys, k.fetched = keys, time.Now()
	return nil
}

func (k *keySet) key(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, refresh := range []bool{false, true} {
		if refresh && time.Since(k.fetched) < keysMinInterval {
			break
		}
		if refresh || k.keys == nil || time.Since(k.fetched) > keysTTL {
			if err := k.fetch(ctx); err != nil {
				return jose.JSONWebKey{}, fmt.Errorf("the keys could not be read: %w", err)
			}
		}
		if key, ok := k.keys[kid]; ok {
			return key, nil
		}
	}
	return jose.JSONWebKey{}, errors.New("signed with an unknown key")
}

// verified are the claims of a token one of the set's keys signed, with one
// of the algorithms named. Nothing of the claims is checked here.
func (k *keySet) verified(ctx context.Context, token string, algorithms []jose.SignatureAlgorithm, claims ...any) error {
	parsed, err := jwt.ParseSigned(token, algorithms)
	if err != nil {
		return err
	}
	if len(parsed.Headers) != 1 {
		return errors.New("not signed once")
	}
	key, err := k.key(ctx, parsed.Headers[0].KeyID)
	if err != nil {
		return err
	}
	return parsed.Claims(key.Key, claims...)
}

// standard are the claims every token must carry and what they must say.
func standard(c jwt.Claims, issuer, audience string, now int64) error {
	if c.Issuer != issuer {
		return errors.New("issued by another party")
	}
	if !c.Audience.Contains(audience) {
		return errors.New("made out to another audience")
	}
	if c.Subject == "" || c.Expiry == nil || c.IssuedAt == nil {
		return errors.New("names no person, no end or no start")
	}
	if c.Expiry.Time().Unix() <= now {
		return errors.New("expired")
	}
	return nil
}
