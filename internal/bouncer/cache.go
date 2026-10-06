/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"sync"
	"time"
)

// cache holds L2 decisions per (subject, session, route): the session, not
// the token, so a refresh is not a miss and load on the store is logins x
// routes (networking.md §4). Eviction, not expiry, is what makes a change
// visible: the poller evicts on every changelog entry.
type cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
	now     func() time.Time
}

type cacheEntry struct {
	identity identity
	expires  time.Time
}

func newCache(ttl time.Duration, now func() time.Time) *cache {
	return &cache{entries: map[string]cacheEntry{}, ttl: ttl, now: now}
}

func cacheKey(sub, sid, host string) string { return sub + "\x00" + sid + "\x00" + host }

func (c *cache) get(sub, sid, host string) (identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[cacheKey(sub, sid, host)]
	if !ok || !c.now().Before(e.expires) {
		return identity{}, false
	}
	return e.identity, true
}

func (c *cache) put(sub, sid, host string, id identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cacheKey(sub, sid, host)] = cacheEntry{identity: id, expires: c.now().Add(c.ttl)}
}

// evictAll forgets every decision. The changelog names what changed, but a
// membership change reaches a subject through groups this cache does not
// model, so the honest answer to any change is to ask again.
func (c *cache) evictAll() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.entries)
	c.entries = map[string]cacheEntry{}
	return n
}
