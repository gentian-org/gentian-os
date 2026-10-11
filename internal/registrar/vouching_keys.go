/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

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

// VouchingKey is one component that vouches for people, as the operator
// lists it for the registrar.
//
// The operator gives such a component a key of its own, in a Secret of the
// tenant's namespace, and writes the key's hash here (the ConfigMap
// registrar-vouching-keys, internal/controller/vouching_keys.go). The
// registrar reads no Secret and never sees the key except when the component
// presents it.
type VouchingKey struct {
	// Tenant is the tenant the component runs in.
	Tenant string `json:"tenant"`
	// Component is the profile the component runs, which is what its entry
	// in the realm is named after.
	Component string `json:"component"`
	// KeyHash is the SHA-256 of the key, in hex.
	KeyHash string `json:"keyHash"`
}

// vouchingKeysFile is the file the operator writes.
type vouchingKeysFile struct {
	Keys []VouchingKey `json:"keys"`
}

var vouchingKeyHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseVouchingKeys reads the operator's list. A list with an entry that
// names no tenant or no component, whose hash is not one, or that shares its
// key with another is refused whole: an entry that cannot be told apart from
// another's is not one to decide by.
func ParseVouchingKeys(raw []byte) ([]VouchingKey, error) {
	var file vouchingKeysFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("vouching keys: %w", err)
	}
	seen := map[string]bool{}
	for i, k := range file.Keys {
		if strings.TrimSpace(k.Tenant) == "" || strings.TrimSpace(k.Component) == "" {
			return nil, fmt.Errorf("vouching keys: entry %d needs tenant and component", i)
		}
		if !vouchingKeyHash.MatchString(k.KeyHash) {
			return nil, fmt.Errorf("vouching keys: entry %d (%s/%s) needs keyHash as 64 hex digits", i, k.Tenant, k.Component)
		}
		if seen[k.KeyHash] {
			return nil, fmt.Errorf("vouching keys: entry %d (%s/%s) shares its key with another", i, k.Tenant, k.Component)
		}
		seen[k.KeyHash] = true
	}
	return file.Keys, nil
}

// LoadVouchingKeys reads the list from a file.
func LoadVouchingKeys(path string) ([]VouchingKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // The path is the deployment's, not a caller's.
	if err != nil {
		return nil, err
	}
	return ParseVouchingKeys(raw)
}

// VouchingKeys is the list the registrar currently holds. The zero value
// holds none, and so does nil: no component is recognised by its key.
type VouchingKeys struct {
	keys atomic.Pointer[[]VouchingKey]
}

// Set replaces the list.
func (v *VouchingKeys) Set(keys []VouchingKey) {
	held := append([]VouchingKey(nil), keys...)
	v.keys.Store(&held)
}

// Len is how many components are listed.
func (v *VouchingKeys) Len() int {
	if v == nil {
		return 0
	}
	if held := v.keys.Load(); held != nil {
		return len(*held)
	}
	return 0
}

// holder returns the component a key belongs to, or nil.
func (v *VouchingKeys) holder(key string) *VouchingKey {
	if v == nil || key == "" {
		return nil
	}
	held := v.keys.Load()
	if held == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(key))
	got := []byte(hex.EncodeToString(sum[:]))
	var found *VouchingKey
	// Every entry is compared, so the time taken says nothing about which
	// one matched or whether one did.
	for i := range *held {
		if subtle.ConstantTimeCompare(got, []byte((*held)[i].KeyHash)) == 1 {
			found = &(*held)[i]
		}
	}
	return found
}

// Follow keeps the list equal to the file until ctx ends.
//
// The file is a mounted ConfigMap: the kubelet swaps it when the operator
// changes it, and the modification time says so. A file that is not there
// is no list, which is where a cluster starts and where it returns when the
// ConfigMap goes. A file that cannot be read as a list keeps the one before
// it, and says so, since dropping every key over one bad entry would stop
// every component at once.
func (v *VouchingKeys) Follow(ctx context.Context, path string, every time.Duration, log *slog.Logger) {
	var last time.Time
	present := false
	load := func() {
		st, err := os.Stat(path)
		if err != nil {
			if present {
				v.Set(nil)
				present = false
				last = time.Time{}
				log.Info("the vouching keys are gone; no component is recognised by its key", "path", path)
			}
			return
		}
		if present && st.ModTime().Equal(last) {
			return
		}
		keys, err := LoadVouchingKeys(path)
		if err != nil {
			log.Warn("vouching keys not reloaded", "path", path, "error", err.Error())
			return
		}
		last, present = st.ModTime(), true
		v.Set(keys)
		log.Info("vouching keys loaded", "path", path, "components", len(keys))
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
