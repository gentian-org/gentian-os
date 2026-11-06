/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package componentkeys is the list of keys components hold, as the operator
// writes it for a service that recognizes a component by its key: the
// director's commission keys, the registrar's vouching keys, the bouncer's
// checkers and the arbiter's table.
//
// The operator gives such a component a key of its own, in a Secret of its
// tenant's namespace, and lists only the key's hash. The service reads no
// Secret and never sees the key except when the component presents it.
package componentkeys

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Key is one component that holds a key, as a list names it. A service's
// entry embeds it with whatever else the service needs of the component.
type Key struct {
	// Tenant is the tenant the component runs in.
	Tenant string `json:"tenant"`
	// Component names the component: the profile it runs, in the director's
	// and the registrar's lists, and the Component, in the bouncer's and the
	// arbiter's.
	Component string `json:"component"`
	// KeyHash is the hex SHA-256 of the key (Hash).
	KeyHash string `json:"keyHash"`
}

func (k Key) key() Key { return k }

// Entry is an entry of a list: a Key, or a struct that embeds one.
type Entry interface{ key() Key }

// Commission is an entry of the director's list: a component that may
// commission partners, and the tenants whose spec.partners it may change.
// Each of them names the component in spec.partnerCommissioners.
type Commission struct {
	Key
	Tenants []string `json:"tenants"`
}

// Hash is the form a list holds a key in: the SHA-256 of the key, in hex.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Check refuses a list with an entry that names no tenant or no component,
// whose hash is not one, or that shares its key with another: an entry that
// cannot be told apart from another's is not one to decide by. what begins
// each refusal and noun names an entry in it. extra, when not nil, is asked
// of each entry after its key and before it is compared with the others.
func Check[E Entry](entries []E, what, noun string, extra func(i int, e E) error) error {
	seen := map[string]bool{}
	for i, e := range entries {
		k := e.key()
		if strings.TrimSpace(k.Tenant) == "" || strings.TrimSpace(k.Component) == "" {
			return fmt.Errorf("%s: %s %d needs tenant and component", what, noun, i)
		}
		if !hashPattern.MatchString(k.KeyHash) {
			return fmt.Errorf("%s: %s %d (%s/%s) needs keyHash as 64 hex digits", what, noun, i, k.Tenant, k.Component)
		}
		if extra != nil {
			if err := extra(i, e); err != nil {
				return err
			}
		}
		if seen[k.KeyHash] {
			return fmt.Errorf("%s: %s %d (%s/%s) shares its key with another", what, noun, i, k.Tenant, k.Component)
		}
		seen[k.KeyHash] = true
	}
	return nil
}

// list is the file the operator writes.
type list[E any] struct {
	Keys []E `json:"keys"`
}

// Parse reads a list the operator wrote (Document), refused whole by Check.
func Parse[E Entry](raw []byte, what string) ([]E, error) {
	var file list[E]
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := Check(file.Keys, what, "entry", nil); err != nil {
		return nil, err
	}
	return file.Keys, nil
}

// Document is a list as Parse reads it. No entries is an empty list, which
// is written all the same: an entry leaves the list by the list being
// written without it.
func Document[E Entry](entries []E) (string, error) {
	if entries == nil {
		entries = []E{}
	}
	b, err := json.Marshal(list[E]{Keys: entries})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Find returns the entry key belongs to, or nil. Every entry is compared, so
// the time taken says nothing about which one matched or whether one did.
func Find[E Entry](entries []E, key string) *E {
	if key == "" {
		return nil
	}
	got := []byte(Hash(key))
	var found *E
	for i := range entries {
		if subtle.ConstantTimeCompare(got, []byte(entries[i].key().KeyHash)) == 1 {
			found = &entries[i]
		}
	}
	return found
}

// List is the list a service currently holds. The zero value holds none, and
// so does nil: no component is recognized by its key.
type List[E Entry] struct {
	held atomic.Pointer[[]E]
}

// Set replaces the list.
func (l *List[E]) Set(entries []E) {
	held := append([]E(nil), entries...)
	l.held.Store(&held)
}

// Len is how many components are listed.
func (l *List[E]) Len() int {
	if l == nil {
		return 0
	}
	if held := l.held.Load(); held != nil {
		return len(*held)
	}
	return 0
}

// Find returns the entry key belongs to, or nil.
func (l *List[E]) Find(key string) *E {
	if l == nil {
		return nil
	}
	held := l.held.Load()
	if held == nil {
		return nil
	}
	return Find(*held, key)
}

// Follow keeps the list equal to the file until ctx ends; what names the list
// in the log and in a refusal.
//
// The file is a mounted ConfigMap: the kubelet swaps it when the operator
// changes it, and the modification time says so. A file that is not there
// is no list, which is where a cluster starts and where it returns when the
// ConfigMap goes. A file that cannot be read as a list keeps the one before
// it, and says so, since dropping every key over one bad entry would stop
// every component at once.
func (l *List[E]) Follow(ctx context.Context, path string, every time.Duration, log *slog.Logger, what string) {
	var last time.Time
	present := false
	load := func() {
		st, err := os.Stat(path)
		if err != nil {
			if present {
				l.Set(nil)
				present, last = false, time.Time{}
				log.Info("the "+what+" are gone; no component is recognized by its key", "path", path)
			}
			return
		}
		if present && st.ModTime().Equal(last) {
			return
		}
		raw, err := os.ReadFile(path) //nolint:gosec // The path is the deployment's, not a caller's.
		if err != nil {
			log.Warn(what+" not reloaded", "path", path, "error", err.Error())
			return
		}
		entries, err := Parse[E](raw, what)
		if err != nil {
			log.Warn(what+" not reloaded", "path", path, "error", err.Error())
			return
		}
		last, present = st.ModTime(), true
		l.Set(entries)
		log.Info(what+" loaded", "path", path, "components", len(entries))
	}
	load()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			load()
		}
	}
}
