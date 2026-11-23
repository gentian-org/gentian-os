/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package membership

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is what the listener inside Keycloak sends: what only the realm sees
// happen to a person.
type Event struct {
	// ID is unique per event; a second delivery of the same id is dropped.
	ID string `json:"id"`
	// Time is when Keycloak saw it, in milliseconds since the epoch.
	Time int64 `json:"time"`
	// Realm is the realm it happened in.
	Realm string `json:"realm"`
	// Type is one of the Type* constants.
	Type string `json:"type"`
	// User is the Keycloak user id -- the sub of that user's tokens.
	User string `json:"user,omitempty"`
	// Provider is the identity provider a person came in through.
	Provider string `json:"provider,omitempty"`
	// ProviderUser is who the person is at that provider: the id of their
	// account there, as the realm's link to it holds it. For a tenant's
	// provider tenant-<source>, the sub of their account in the source
	// tenant's realm. Absent when the realm did not say.
	ProviderUser string `json:"providerUser,omitempty"`
}

const (
	// TypeUserEntered states that a person signed in to the realm through an
	// identity provider: an account of another realm, let in here.
	TypeUserEntered = "user.entered"
	// TypeUserDeleted states that a user no longer exists.
	TypeUserDeleted = "user.deleted"
	// TypeUserRegistered states that a person made an account in the realm
	// themself, on its registration page.
	TypeUserRegistered = "user.registered"
)

// SignatureHeader carries "keyid=<id>,t=<unix seconds>,sig=<base64>", where
// sig is an Ed25519 signature over "<t>.<body>". The listener holds the private
// key and the platform only the public one, so nothing the platform stores or
// leaks lets anyone speak as Keycloak.
const SignatureHeader = "X-Gentian-Signature"

const (
	// window is how far a signature's timestamp may be from now. It bounds how
	// long a captured event stays replayable, and so how much the seen-id set
	// must remember.
	window = 5 * time.Minute
	// maxEventBytes caps an event body.
	maxEventBytes = 16 << 10
)

var (
	eventID      = regexp.MustCompile(`^[A-Za-z0-9._-]{8,80}$`)
	realmName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	providerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// providerUser is what an account id at a provider may be: printable,
	// without spaces, and bounded.
	providerUser = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)
)

// Arrivals is told of a person who came into a realm through an identity
// provider, so that what the platform gives such a person is theirs when
// they arrive. providerUser is who they are at the provider, or "" when the realm did
// not say.
type Arrivals interface {
	PersonEntered(ctx context.Context, realm, sub, provider, providerUser string) error
}

// Registrations is told of a person who made an account in a realm
// themself, so that what the platform gives such an account is theirs from
// the start. An Arrivals that is also a Registrations is told both.
type Registrations interface {
	PersonRegistered(ctx context.Context, realm, sub string) error
}

// Departures is told of a person who no longer exists in a realm, so that
// what they held, and what is kept for that person alone, ends with them.
type Departures interface {
	PersonRemoved(ctx context.Context, realm, sub string) error
}

// Receiver is the HTTP endpoint events arrive at.
type Receiver struct {
	keys          map[string]ed25519.PublicKey
	arrivals      Arrivals
	registrations Registrations
	departures    []Departures
	log           *slog.Logger
	now           func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // event id → when it may be forgotten
}

// NewReceiver returns a Receiver that accepts events signed by any of keys,
// which maps a key id to a public key. More than one key is how a key is
// rotated without losing events. arrivals may be nil, and departures empty:
// such an event is then accepted and nothing follows from it. When arrivals
// is also a Registrations, it is told of the accounts people make
// themselves; otherwise nothing follows from those either.
func NewReceiver(keys map[string]ed25519.PublicKey, arrivals Arrivals, log *slog.Logger, departures ...Departures) (*Receiver, error) {
	if len(keys) == 0 {
		return nil, errors.New("membership: at least one listener key is required")
	}
	if log == nil {
		log = slog.Default()
	}
	registrations, _ := arrivals.(Registrations)
	return &Receiver{keys: keys, arrivals: arrivals, registrations: registrations, departures: departures, log: log,
		now: time.Now, seen: map[string]time.Time{}}, nil
}

// ParseKeys reads "id=base64key,id=base64key".
func ParseKeys(s string) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		id, enc, ok := strings.Cut(item, "=")
		if !ok || id == "" {
			return nil, fmt.Errorf("membership: listener key %q is not id=key", item)
		}
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("membership: listener key %q is not a base64 Ed25519 public key", id)
		}
		keys[id] = ed25519.PublicKey(raw)
	}
	return keys, nil
}

// ServeHTTP implements http.Handler.
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEventBytes+1))
	if err != nil || len(body) > maxEventBytes {
		http.Error(w, "event too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := rc.verify(r.Header.Get(SignatureHeader), body); err != nil {
		rc.log.WarnContext(ctx, "realm event refused", "reason", err.Error())
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || !eventID.MatchString(ev.ID) || !realmName.MatchString(ev.Realm) {
		http.Error(w, "malformed event", http.StatusBadRequest)
		return
	}
	if !rc.first(ev.ID) {
		w.WriteHeader(http.StatusNoContent) // delivered before; the sender may stop retrying
		return
	}
	switch ev.Type {
	case TypeUserEntered:
		if ev.User == "" || !providerName.MatchString(ev.Provider) ||
			(ev.ProviderUser != "" && !providerUser.MatchString(ev.ProviderUser)) {
			http.Error(w, "malformed event", http.StatusBadRequest)
			return
		}
		if rc.arrivals != nil {
			err = rc.arrivals.PersonEntered(ctx, ev.Realm, ev.User, ev.Provider, ev.ProviderUser)
		}
	case TypeUserRegistered:
		if ev.User == "" {
			http.Error(w, "malformed event", http.StatusBadRequest)
			return
		}
		if rc.registrations != nil {
			err = rc.registrations.PersonRegistered(ctx, ev.Realm, ev.User)
		}
	case TypeUserDeleted:
		if ev.User == "" {
			http.Error(w, "malformed event", http.StatusBadRequest)
			return
		}
		for _, d := range rc.departures {
			if d == nil {
				continue
			}
			if derr := d.PersonRemoved(ctx, ev.Realm, ev.User); derr != nil && err == nil {
				err = derr
			}
		}
	default:
		// A listener newer than this operator may send types it does not know.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		// Forget the id so the listener's retry is not mistaken for a replay.
		rc.forget(ev.ID)
		rc.log.ErrorContext(ctx, "realm event not applied", "event", ev.ID, "realm", ev.Realm, "error", err.Error())
		http.Error(w, "not applied", http.StatusServiceUnavailable)
		return
	}
	rc.log.InfoContext(ctx, "realm event applied", "event", ev.ID, "realm", ev.Realm, "type", ev.Type,
		"user", ev.User, "provider", ev.Provider, "provider_user", ev.ProviderUser)
	w.WriteHeader(http.StatusNoContent)
}

func (rc *Receiver) verify(header string, body []byte) error {
	return VerifySignature(rc.keys, header, body, rc.now())
}

// VerifySignature checks a SignatureHeader over body against keys at now:
// a listed key, a timestamp inside the window, and a signature that
// verifies. It is how anything in the cluster knows a request is Keycloak's.
func VerifySignature(keys map[string]ed25519.PublicKey, header string, body []byte, now time.Time) error {
	fields := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			fields[k] = v
		}
	}
	key, ok := keys[fields["keyid"]]
	if !ok {
		return errors.New("unknown key id")
	}
	ts, err := strconv.ParseInt(fields["t"], 10, 64)
	if err != nil {
		return errors.New("no timestamp")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > window || d < -window {
		return errors.New("timestamp outside the window")
	}
	sig, err := base64.StdEncoding.DecodeString(fields["sig"])
	if err != nil {
		return errors.New("signature is not base64")
	}
	msg := append([]byte(fields["t"]+"."), body...)
	if !ed25519.Verify(key, msg, sig) {
		return errors.New("signature does not verify")
	}
	return nil
}

// first records an event id and reports whether it is new. Ids are kept for
// twice the signature window: past that, the timestamp check refuses a replay
// on its own.
func (rc *Receiver) first(id string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := rc.now()
	for k, until := range rc.seen {
		if now.After(until) {
			delete(rc.seen, k)
		}
	}
	if _, dup := rc.seen[id]; dup {
		return false
	}
	rc.seen[id] = now.Add(2 * window)
	return true
}

func (rc *Receiver) forget(id string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.seen, id)
}
