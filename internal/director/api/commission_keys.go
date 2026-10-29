/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

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
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// CommissionKey is one component that may commission partners, as the
// operator lists it for the director (the ConfigMap director-commission-keys,
// internal/controller/partner_commissioners.go).
//
// The operator gives the component a key of its own, in a Secret of its
// tenant's namespace, and writes the key's hash here with the tenants whose
// realm it may add an outside realm to. The director reads no Secret and
// never sees the key except when the component presents it.
type CommissionKey struct {
	// Tenant is the tenant the component runs in.
	Tenant string `json:"tenant"`
	// Component is the profile it runs.
	Component string `json:"component"`
	// KeyHash is the SHA-256 of the key, in hex.
	KeyHash string `json:"keyHash"`
	// Tenants are the tenants whose spec.partners it may change: each
	// names the component in spec.partnerCommissioners.
	Tenants []string `json:"tenants"`
}

// may reports whether the key may change tenant's outside realms.
func (k *CommissionKey) may(tenant string) bool {
	return k != nil && slices.Contains(k.Tenants, tenant)
}

var commissionKeyHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseCommissionKeys reads the operator's list. A list with an entry that
// names no tenant or component, whose hash is not one, or that shares its key
// with another is refused whole, as the registrar refuses its own.
func ParseCommissionKeys(raw []byte) ([]CommissionKey, error) {
	var file struct {
		Keys []CommissionKey `json:"keys"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("commission keys: %w", err)
	}
	seen := map[string]bool{}
	for i, k := range file.Keys {
		if strings.TrimSpace(k.Tenant) == "" || strings.TrimSpace(k.Component) == "" {
			return nil, fmt.Errorf("commission keys: entry %d needs tenant and component", i)
		}
		if !commissionKeyHash.MatchString(k.KeyHash) {
			return nil, fmt.Errorf("commission keys: entry %d (%s/%s) needs keyHash as 64 hex digits", i, k.Tenant, k.Component)
		}
		if seen[k.KeyHash] {
			return nil, fmt.Errorf("commission keys: entry %d (%s/%s) shares its key with another", i, k.Tenant, k.Component)
		}
		seen[k.KeyHash] = true
	}
	return file.Keys, nil
}

// CommissionKeys is the list the director currently holds. The zero value,
// and nil, hold none: no component is recognized by its key.
type CommissionKeys struct {
	keys atomic.Pointer[[]CommissionKey]
}

// Set replaces the list.
func (v *CommissionKeys) Set(keys []CommissionKey) {
	held := append([]CommissionKey(nil), keys...)
	v.keys.Store(&held)
}

// holder returns the component a key belongs to, or nil. Every entry is
// compared, so the time taken says nothing about which one matched.
func (v *CommissionKeys) holder(key string) *CommissionKey {
	if v == nil || key == "" {
		return nil
	}
	held := v.keys.Load()
	if held == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(key))
	got := []byte(hex.EncodeToString(sum[:]))
	var found *CommissionKey
	for i := range *held {
		if subtle.ConstantTimeCompare(got, []byte((*held)[i].KeyHash)) == 1 {
			found = &(*held)[i]
		}
	}
	return found
}

// Follow keeps the list equal to the file until ctx ends. The file is a
// mounted ConfigMap; one that is not there is no list, and one that cannot
// be read keeps the list before it, and says so.
func (v *CommissionKeys) Follow(ctx context.Context, path string, every time.Duration, log *slog.Logger) {
	var last time.Time
	present := false
	load := func() {
		st, err := os.Stat(path)
		if err != nil {
			if present {
				v.Set(nil)
				present, last = false, time.Time{}
				log.Info("the commission keys are gone; no component is recognized by its key", "path", path)
			}
			return
		}
		if present && st.ModTime().Equal(last) {
			return
		}
		raw, err := os.ReadFile(path) //nolint:gosec // The path is the deployment's, not a caller's.
		if err != nil {
			log.Warn("commission keys not reloaded", "path", path, "error", err.Error())
			return
		}
		keys, err := ParseCommissionKeys(raw)
		if err != nil {
			log.Warn("commission keys not reloaded", "path", path, "error", err.Error())
			return
		}
		last, present = st.ModTime(), true
		v.Set(keys)
		log.Info("commission keys loaded", "path", path, "components", len(keys))
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
