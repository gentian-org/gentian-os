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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Device passwords at a door.
//
// A calendar, contact or file program that knows only a username and a
// password presents them as HTTP Basic credentials. The door does not check
// them itself: it posts them to the tenant's realm as a password grant of the
// realm's door device client (gentian-door-devices), whose one sign-in step
// accepts a device password and nothing else (the platform's Keycloak
// extension). The realm answers with an access token of the person made out
// to the door, carrying the ways the password covers (gentian_ways). From
// there the request is decided exactly as one that presented that token: the
// same verification, the same question, the same exchange for the app's own
// token. The one thing added is that the path's way must be among the
// password's.
//
// A token is kept for the credentials it was obtained with until shortly
// before it runs out, so the realm is asked once in a few minutes per device
// and not on every request of a sync. A failure is not kept; failures are
// counted instead, per username and per source, and a caller past the limit
// is answered 429 without the realm being asked.

// DevicesClientID is the client every tenant realm checks device passwords
// with (crossplane/compositions/tenant-default.yaml).
const DevicesClientID = "gentian-door-devices"

// devicesSecretSuffix names a realm's device client secret in the secrets
// directory: <realm>.devices (internal/controller, devicesEntrySuffix: the
// two must agree).
const devicesSecretSuffix = ".devices"

// DeviceToken is what a device password was exchanged for.
type DeviceToken struct {
	Token   string
	Ways    []string
	Expires time.Time
}

// DeviceRealm obtains a person's token for a device password.
type DeviceRealm interface {
	// Offers reports whether the realm takes device passwords at all: its
	// device client's secret is there.
	Offers(realm string) bool
	// Grant posts a username and a device password to the realm. An error
	// that is ErrDeviceRefused is the realm saying no; any other is the
	// realm not answering.
	Grant(ctx context.Context, realm, username, password string) (DeviceToken, error)
}

// ErrDeviceRefused is a device password the realm did not accept.
var ErrDeviceRefused = errors.New("the realm refused the device password")

// RealmDevices asks Keycloak's token endpoint.
type RealmDevices struct {
	// TokenBase and SecretsDir as RealmExchanger's.
	TokenBase  string
	SecretsDir string
	Client     *http.Client
	Now        func() time.Time
}

func (r *RealmDevices) secret(realm string) ([]byte, error) {
	if !realmNamePattern.MatchString(realm) {
		return nil, fmt.Errorf("realm %q is not a name", realm)
	}
	return os.ReadFile(filepath.Join(r.SecretsDir, realm+devicesSecretSuffix))
}

// Offers implements DeviceRealm.
func (r *RealmDevices) Offers(realm string) bool {
	s, err := r.secret(realm)
	return err == nil && len(strings.TrimSpace(string(s))) > 0
}

// Grant implements DeviceRealm.
func (r *RealmDevices) Grant(ctx context.Context, realm, username, password string) (DeviceToken, error) {
	secret, err := r.secret(realm)
	if err != nil {
		return DeviceToken{}, fmt.Errorf("no device client secret for realm %s: %w", realm, err)
	}
	form := url.Values{
		"grant_type":    {"password"},
		"client_id":     {DevicesClientID},
		"client_secret": {strings.TrimSpace(string(secret))},
		"username":      {username},
		"password":      {password},
	}
	endpoint := strings.TrimRight(r.TokenBase, "/") + "/realms/" + realm + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return DeviceToken{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeviceToken{}, err
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return DeviceToken{}, fmt.Errorf("the realm answered %d with no JSON", resp.StatusCode)
	}
	// invalid_grant is the realm's no: a wrong, idle or removed password, or
	// a person who is disabled. Anything else is not an answer about the
	// password.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest && answer.Error == "invalid_grant" {
		return DeviceToken{}, ErrDeviceRefused
	}
	if resp.StatusCode != http.StatusOK || answer.AccessToken == "" {
		return DeviceToken{}, fmt.Errorf("the realm answered the device password grant with %d %s", resp.StatusCode, answer.Error)
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	return DeviceToken{
		Token:   answer.AccessToken,
		Ways:    waysOf(answer.AccessToken),
		Expires: now().Add(time.Duration(answer.ExpiresIn) * time.Second),
	}, nil
}

// waysOf reads gentian_ways from a token the realm just handed over its own
// connection. The token is verified as any other before it is used; the
// ways are read here only to be kept with it.
func waysOf(token string) []string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Ways string `json:"gentian_ways"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return strings.Fields(claims.Ways)
}

// basicAuth reads HTTP Basic credentials.
func basicAuth(h string) (user, password string, ok bool) {
	const prefix = "basic "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	user, password, ok = strings.Cut(string(raw), ":")
	if !ok || user == "" || password == "" {
		return "", "", false
	}
	return user, password, true
}

// Limits on failed device passwords, per username of a realm and per source.
const (
	deviceFailureWindow   = 10 * time.Minute
	deviceFailuresPerUser = 10
	deviceFailuresPerAddr = 30
	// deviceTokenMargin is how long before it runs out a kept token stops
	// being handed on, so that it does not run out on its way to the app.
	deviceTokenMargin = 30 * time.Second
)

// devices keeps the tokens device passwords were exchanged for and counts
// the failures.
type devices struct {
	realm DeviceRealm
	now   func() time.Time

	mu       sync.Mutex
	tokens   map[[32]byte]DeviceToken
	failures map[string][]time.Time
}

func newDevices(realm DeviceRealm, now func() time.Time) *devices {
	if realm == nil {
		return nil
	}
	return &devices{realm: realm, now: now, tokens: map[[32]byte]DeviceToken{}, failures: map[string][]time.Time{}}
}

// errDeviceLimited is a caller past the limit of failed attempts.
var errDeviceLimited = errors.New("too many failed device passwords")

// token is the person's token for a device password, from the ones kept or
// from the realm.
func (d *devices) token(ctx context.Context, realm, user, password, source string) (DeviceToken, error) {
	key := sha256.Sum256([]byte(realm + "\x00" + user + "\x00" + password))
	userKey, addrKey := "u\x00"+realm+"\x00"+strings.ToLower(user), "a\x00"+source
	now := d.now()

	d.mu.Lock()
	if t, ok := d.tokens[key]; ok && now.Before(t.Expires.Add(-deviceTokenMargin)) {
		d.mu.Unlock()
		return t, nil
	}
	delete(d.tokens, key)
	if d.count(userKey, now) >= deviceFailuresPerUser || (source != "" && d.count(addrKey, now) >= deviceFailuresPerAddr) {
		d.mu.Unlock()
		return DeviceToken{}, errDeviceLimited
	}
	d.mu.Unlock()

	t, err := d.realm.Grant(ctx, realm, user, password)

	d.mu.Lock()
	defer d.mu.Unlock()
	if errors.Is(err, ErrDeviceRefused) {
		d.failures[userKey] = append(d.failures[userKey], now)
		if source != "" {
			d.failures[addrKey] = append(d.failures[addrKey], now)
		}
		return DeviceToken{}, err
	}
	if err != nil {
		return DeviceToken{}, err
	}
	d.sweep(now)
	d.tokens[key] = t
	return t, nil
}

// count is the failures under key within the window. Called with mu held.
func (d *devices) count(key string, now time.Time) int {
	kept := d.failures[key][:0]
	for _, at := range d.failures[key] {
		if now.Sub(at) < deviceFailureWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(d.failures, key)
		return 0
	}
	d.failures[key] = kept
	return len(kept)
}

// sweep drops the tokens that ran out. Called with mu held.
func (d *devices) sweep(now time.Time) {
	for k, t := range d.tokens {
		if !now.Before(t.Expires) {
			delete(d.tokens, k)
		}
	}
}

// covers reports whether a password's ways include one that serves the path.
func covers(have, serving []string) bool {
	for _, w := range serving {
		for _, h := range have {
			if w == h {
				return true
			}
		}
	}
	return false
}
