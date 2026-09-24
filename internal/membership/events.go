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

package membership

import (
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

// Event is what the listener inside Keycloak sends.
type Event struct {
	// ID is unique per event; a second delivery of the same id is dropped.
	ID string `json:"id"`
	// Time is when Keycloak made the change, in milliseconds since the epoch.
	Time int64 `json:"time"`
	// Realm is the realm the change happened in.
	Realm string `json:"realm"`
	// Type is one of the Type* constants.
	Type string `json:"type"`
	// User is the Keycloak user id — the sub of that user's tokens.
	User string `json:"user,omitempty"`
	// Groups is the user's complete set of group names after the change.
	Groups []string `json:"groups,omitempty"`
	// Group is the name of a deleted group.
	Group string `json:"group,omitempty"`
}

const (
	// TypeUserMemberships states a user's complete group set.
	TypeUserMemberships = "user.memberships"
	// TypeUserDeleted states that a user no longer exists.
	TypeUserDeleted = "user.deleted"
	// TypeGroupDeleted states that a group no longer exists.
	TypeGroupDeleted = "group.deleted"
)

// SignatureHeader carries "keyid=<id>,t=<unix seconds>,sig=<base64>", where
// sig is an Ed25519 signature over "<t>.<body>". The listener holds the private
// key and the director only the public one, so nothing the director stores or
// leaks lets anyone speak as Keycloak.
const SignatureHeader = "X-Gentian-Signature"

const (
	// window is how far a signature's timestamp may be from now. It bounds how
	// long a captured event stays replayable, and so how much the seen-id set
	// must remember.
	window = 5 * time.Minute
	// maxEventBytes caps an event body. A user in a thousand groups fits.
	maxEventBytes = 256 << 10
)

var (
	eventID   = regexp.MustCompile(`^[A-Za-z0-9._-]{8,80}$`)
	realmName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
)

// Receiver is the HTTP endpoint events arrive at.
type Receiver struct {
	keys map[string]ed25519.PublicKey
	proj *Projector
	log  *slog.Logger
	now  func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // event id → when it may be forgotten
	last map[string]int64     // realm/user → time of the newest event applied
}

// NewReceiver returns a Receiver that accepts events signed by any of keys,
// which maps a key id to a public key. More than one key is how a key is
// rotated without losing events.
func NewReceiver(keys map[string]ed25519.PublicKey, proj *Projector, log *slog.Logger) (*Receiver, error) {
	if len(keys) == 0 || proj == nil {
		return nil, errors.New("membership: at least one listener key and a projector are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Receiver{keys: keys, proj: proj, log: log, now: time.Now,
		seen: map[string]time.Time{}, last: map[string]int64{}}, nil
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
		rc.log.WarnContext(ctx, "membership event refused", "reason", err.Error())
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

	var change Change
	switch ev.Type {
	case TypeUserMemberships, TypeUserDeleted:
		if ev.User == "" {
			http.Error(w, "malformed event", http.StatusBadRequest)
			return
		}
		if !rc.newest(ev.Realm+"/"+ev.User, ev.Time) {
			// State, not deltas: an older statement about a user we already
			// hold a newer one for is simply out of date.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		groups := ev.Groups
		if ev.Type == TypeUserDeleted {
			groups = nil
		}
		change, err = rc.proj.SetUserGroups(ctx, ev.Realm, ev.User, groups)
	case TypeGroupDeleted:
		change, err = rc.proj.RemoveGroup(ctx, ev.Realm, ev.Group)
	default:
		// A listener newer than this director may send types it does not know.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		// Forget the id so the listener's retry is not mistaken for a replay.
		rc.forget(ev.ID, ev.Realm+"/"+ev.User)
		rc.log.ErrorContext(ctx, "membership event not applied", "event", ev.ID, "realm", ev.Realm, "error", err.Error())
		http.Error(w, "not applied", http.StatusServiceUnavailable)
		return
	}
	rc.log.InfoContext(ctx, "membership event applied", "event", ev.ID, "realm", ev.Realm, "type", ev.Type,
		"user", ev.User, "group", ev.Group, "added", change.Added, "removed", change.Removed, "refused", change.Refused)
	w.WriteHeader(http.StatusNoContent)
}

func (rc *Receiver) verify(header string, body []byte) error {
	fields := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			fields[k] = v
		}
	}
	key, ok := rc.keys[fields["keyid"]]
	if !ok {
		return errors.New("unknown key id")
	}
	ts, err := strconv.ParseInt(fields["t"], 10, 64)
	if err != nil {
		return errors.New("no timestamp")
	}
	if d := rc.now().Sub(time.Unix(ts, 0)); d > window || d < -window {
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

func (rc *Receiver) newest(key string, t int64) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if t < rc.last[key] {
		return false
	}
	// Nothing older than the signature window can arrive, so entries that old
	// no longer decide anything.
	if len(rc.last) > 4096 {
		floor := rc.now().Add(-2 * window).UnixMilli()
		for k, v := range rc.last {
			if v < floor {
				delete(rc.last, k)
			}
		}
	}
	rc.last[key] = t
	return true
}

func (rc *Receiver) forget(id, key string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.seen, id)
	delete(rc.last, key)
}
