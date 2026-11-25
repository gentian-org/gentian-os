/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package liaison keeps a person's connections to services outside the
// cluster: the tool servers the tenant's agent bridges name with a sign-in.
//
// A person connects on the liaison's own page: they are sent to the service,
// sign in there, and the liaison is handed a credential it may present for
// them afterwards. The lasting part of it is kept by the platform's
// confidant, under the person, and nowhere else: the liaison has no
// database. The chaperone asks the liaison for a short credential when it
// carries a call for the person, and is never handed the lasting one.
package liaison

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Connection is an outside tool server people sign in at, as the chaperone
// tells the liaison of it.
type Connection struct {
	// Name is the agent bridge's name in the tenant.
	Name string `json:"name"`
	// URL is the tool server: what a credential is asked for.
	URL string `json:"url"`
	// Issuer is the authorization server people sign in at.
	Issuer string `json:"issuer"`
	// Scopes are asked for at sign-in.
	Scopes []string `json:"scopes,omitempty"`
}

var connectionName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

func (c Connection) valid() bool {
	return connectionName.MatchString(c.Name) && secure(c.URL) && secure(c.Issuer)
}

// kept is what the confidant keeps for one person and one connection.
type kept struct {
	Client     Client `json:"client"`
	Token      string `json:"token_endpoint"`
	Revocation string `json:"revocation_endpoint,omitempty"`
	Resource   string `json:"resource"`
	Refresh    string `json:"refresh_token,omitempty"`
	Access     string `json:"access_token"`
	// Expires is when Access runs out, in seconds since the epoch; 0 where
	// the service did not say.
	Expires int64 `json:"expires_at"`
}

// Confidant is where the lasting part of a connection is kept.
type Confidant interface {
	// Put keeps value for person under name. personToken is the person's
	// own token, needed for the first entry kept for them, or "".
	Put(ctx context.Context, person, name, value, personToken string) error
	// Get reads it; found is false where nothing is kept.
	Get(ctx context.Context, person, name string) (value string, found bool, err error)
	Delete(ctx context.Context, person, name string) error
}

// ErrNotConnected is a person who has no connection, or whose connection
// the service no longer honors: they have to connect again on the page.
var ErrNotConnected = errors.New("this person is not connected to the service")

// pending is a sign-in a person started and has not come back from.
type pending struct {
	person     string
	connection Connection
	server     Authority
	client     Client
	verifier   string
	redirect   string
	// desktop: the sign-in was started in the desktop, and the person goes
	// back there.
	desktop bool
	at      time.Time
}

// pendingFor is how long a person has to come back from the service.
const pendingFor = 10 * time.Minute

// Service is the liaison's logic.
type Service struct {
	OAuth     *OAuth
	Confidant Confidant
	// ClientName is the name the liaison registers under at a service.
	ClientName string
	Now        func() time.Time

	mu          sync.Mutex
	connections map[string]Connection
	waiting     map[string]pending
	// renewing serializes renewals per person and connection: a service
	// that rotates the lasting credential honors each one once.
	renewing map[string]*sync.Mutex
}

// NewService returns a Service.
func NewService(oauth *OAuth, confidant Confidant, clientName string) *Service {
	return &Service{
		OAuth: oauth, Confidant: confidant, ClientName: clientName, Now: time.Now,
		connections: map[string]Connection{}, waiting: map[string]pending{}, renewing: map[string]*sync.Mutex{},
	}
}

// Tell replaces the connections the liaison knows of. One that is not a
// name with two https addresses is left out.
func (s *Service) Tell(connections []Connection) int {
	known := map[string]Connection{}
	for _, c := range connections {
		if c.valid() {
			known[c.Name] = c
		}
	}
	s.mu.Lock()
	s.connections = known
	s.mu.Unlock()
	return len(known)
}

// Connections are the known connections, by name.
func (s *Service) Connections() []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]Connection, 0, len(s.connections))
	for _, c := range s.connections {
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all
}

func (s *Service) connection(name string) (Connection, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.connections[name]
	return c, ok
}

// Connected reports whether something is kept for person at connection.
func (s *Service) Connected(ctx context.Context, person, connection string) (bool, error) {
	_, found, err := s.Confidant.Get(ctx, person, connection)
	return found, err
}

// Begin starts a sign-in of person at connection and answers where to send
// them. redirect is the liaison's own address a sign-in comes back to;
// desktop says the person started it in the desktop and goes back there.
func (s *Service) Begin(ctx context.Context, person, name, redirect string, desktop bool) (string, error) {
	connection, ok := s.connection(name)
	if !ok {
		return "", fmt.Errorf("no installed agent bridge is named %s, or it names no sign-in", name)
	}
	server, err := s.OAuth.Discover(ctx, connection.Issuer)
	if err != nil {
		return "", err
	}
	client, err := s.OAuth.Register(ctx, server, s.ClientName, redirect)
	if err != nil {
		return "", err
	}
	state, verifier := random(24), Verifier()
	now := s.Now()
	s.mu.Lock()
	for old, p := range s.waiting {
		if now.Sub(p.at) > pendingFor {
			delete(s.waiting, old)
		}
	}
	s.waiting[state] = pending{person: person, connection: connection, server: server, client: client,
		verifier: verifier, redirect: redirect, desktop: desktop, at: now}
	s.mu.Unlock()
	return AuthorizeURL(server, client, redirect, state, verifier, connection.URL, connection.Scopes), nil
}

// Complete finishes the sign-in state names, for the person who came back
// with it. personToken is that person's own token: with it the confidant
// keeps the first entry for them. desktop says where the person goes back
// to, and is known whenever the sign-in was started here by this person.
func (s *Service) Complete(ctx context.Context, person, personToken, state, code string) (name string, desktop bool, err error) {
	p, ok := s.take1(state)
	if !ok || s.Now().Sub(p.at) > pendingFor || code == "" {
		return "", false, errors.New("this sign-in was not started here, or took too long: start it again")
	}
	if p.person != person {
		return "", false, errors.New("this sign-in was started by somebody else")
	}
	grant, err := s.OAuth.Exchange(ctx, p.server.Token, p.client, code, p.verifier, p.redirect, p.connection.URL)
	if err != nil {
		return "", p.desktop, err
	}
	entry := kept{Client: p.client, Token: p.server.Token, Revocation: p.server.Revocation, Resource: p.connection.URL}
	s.take(&entry, grant)
	if err := s.keep(ctx, person, p.connection.Name, entry, personToken); err != nil {
		return "", p.desktop, err
	}
	return p.connection.Name, p.desktop, nil
}

// Forget drops the sign-in state names, which the service turned down, and
// says whether this person started it in the desktop.
func (s *Service) Forget(person, state string) (desktop bool) {
	p, ok := s.take1(state)
	return ok && p.person == person && p.desktop
}

// take1 removes and returns the sign-in state names.
func (s *Service) take1(state string) (pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.waiting[state]
	delete(s.waiting, state)
	return p, ok
}

// take puts what a token endpoint answered into entry. A service that hands
// no new lasting credential leaves the one held.
func (s *Service) take(entry *kept, grant Grant) {
	entry.Access = grant.AccessToken
	if grant.RefreshToken != "" {
		entry.Refresh = grant.RefreshToken
	}
	entry.Expires = 0
	if grant.ExpiresIn > 0 {
		entry.Expires = s.Now().Unix() + grant.ExpiresIn
	}
}

func (s *Service) keep(ctx context.Context, person, name string, entry kept, personToken string) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return s.Confidant.Put(ctx, person, name, string(raw), personToken)
}

func (s *Service) read(ctx context.Context, person, name string) (kept, error) {
	var entry kept
	raw, found, err := s.Confidant.Get(ctx, person, name)
	if err != nil {
		return entry, err
	}
	if !found || json.Unmarshal([]byte(raw), &entry) != nil || entry.Access == "" {
		return entry, ErrNotConnected
	}
	return entry, nil
}

// renewEarly is how long before an access credential runs out it is
// renewed, so that one handed out lasts for the call it is for.
const renewEarly = 2 * time.Minute

// Credential is a short credential to call connection's tool server as
// person, and when it runs out (0 where the service did not say). The
// lasting credential never leaves the liaison.
func (s *Service) Credential(ctx context.Context, person, name string) (string, int64, error) {
	connection, ok := s.connection(name)
	if !ok {
		return "", 0, fmt.Errorf("no installed agent bridge is named %s, or it names no sign-in", name)
	}
	lock := s.lockFor(person + "/" + name)
	lock.Lock()
	defer lock.Unlock()

	entry, err := s.read(ctx, person, name)
	if err != nil {
		return "", 0, err
	}
	// What is kept is for the server it was obtained for: an entry whose
	// address changed since is not presented at the new one.
	if entry.Resource != connection.URL {
		return "", 0, ErrNotConnected
	}
	if entry.Expires == 0 || s.Now().Add(renewEarly).Unix() < entry.Expires {
		return entry.Access, entry.Expires, nil
	}
	if entry.Refresh == "" {
		return "", 0, ErrNotConnected
	}
	grant, err := s.OAuth.Refresh(ctx, entry.Token, entry.Client, entry.Refresh, entry.Resource)
	if errors.Is(err, ErrGrantRefused) {
		return "", 0, ErrNotConnected
	}
	if err != nil {
		return "", 0, err
	}
	s.take(&entry, grant)
	// The person is away: the entry is theirs already, and is replaced with
	// the liaison's key alone.
	if err := s.keep(ctx, person, name, entry, ""); err != nil {
		return "", 0, err
	}
	return entry.Access, entry.Expires, nil
}

func (s *Service) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renewing[key] == nil {
		s.renewing[key] = &sync.Mutex{}
	}
	return s.renewing[key]
}

// Disconnect gives up person's connection: the service is told where it
// takes that, and what was kept is removed either way.
func (s *Service) Disconnect(ctx context.Context, person, name string) error {
	if !connectionName.MatchString(name) {
		return errors.New("that is not a connection's name")
	}
	if entry, err := s.read(ctx, person, name); err == nil {
		token := entry.Refresh
		if token == "" {
			token = entry.Access
		}
		// Best effort: a service that cannot be told must not keep the
		// person connected here.
		_ = s.OAuth.Revoke(ctx, entry.Revocation, entry.Client, token)
	}
	return s.Confidant.Delete(ctx, person, name)
}

// ConfidantClient asks the platform's confidant, with the liaison's own
// component key.
type ConfidantClient struct {
	base string
	key  string
	http *http.Client
}

// NewConfidantClient asks the confidant at base with key.
func NewConfidantClient(base, key string, client *http.Client) *ConfidantClient {
	if client == nil {
		client = &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &ConfidantClient{base: strings.TrimRight(base, "/"), key: key, http: client}
}

func (c *ConfidantClient) do(ctx context.Context, method, person, name string, body []byte, personToken string) (*http.Response, error) {
	address := c.base + "/v1/people/" + url.PathEscape(person) + "/secrets/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if personToken != "" {
		req.Header.Set("Gentian-Person-Token", personToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the confidant could not be asked: %w", err)
	}
	return resp, nil
}

// Put implements Confidant.
func (c *ConfidantClient) Put(ctx context.Context, person, name, value, personToken string) error {
	body, _ := json.Marshal(map[string]string{"value": value})
	resp, err := c.do(ctx, http.MethodPut, person, name, body, personToken)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the confidant answered %d", resp.StatusCode)
	}
	return nil
}

// Get implements Confidant.
func (c *ConfidantClient) Get(ctx context.Context, person, name string) (string, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, person, name, nil, "")
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("the confidant answered %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", false, fmt.Errorf("the confidant's answer could not be read: %w", err)
	}
	return out.Value, out.Value != "", nil
}

// Delete implements Confidant.
func (c *ConfidantClient) Delete(ctx context.Context, person, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, person, name, nil, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the confidant answered %d", resp.StatusCode)
	}
	return nil
}

// NoConfidant stands in on a platform that keeps no secrets of people:
// nothing is kept and nothing is found, and every use says why.
type NoConfidant struct{}

var errNoConfidant = errors.New("this platform keeps no secrets of people: connections need a platform with the confidant")

// Put implements Confidant.
func (NoConfidant) Put(context.Context, string, string, string, string) error { return errNoConfidant }

// Get implements Confidant.
func (NoConfidant) Get(context.Context, string, string) (string, bool, error) {
	return "", false, errNoConfidant
}

// Delete implements Confidant.
func (NoConfidant) Delete(context.Context, string, string) error { return errNoConfidant }
