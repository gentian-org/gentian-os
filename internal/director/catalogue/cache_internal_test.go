/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package catalogue

import (
	"testing"
	"time"
)

// A fetcher reuses what it fetched for five minutes, unless it was told a
// shorter (or longer) time: a development cluster that follows a channel
// published many times a day wants the new index within seconds.
func TestAFetcherReusesAnIndexAsLongAsItWasTold(t *testing.T) {
	f := NewFetcher()
	if got := f.cacheFor(); got != indexTTL {
		t.Fatalf("by default %v, want %v", got, indexTTL)
	}
	f.CacheFor = 30 * time.Second
	if got := f.cacheFor(); got != 30*time.Second {
		t.Fatalf("told 30s, got %v", got)
	}
	f.store("dev", cachedIndex{at: time.Now().Add(-45 * time.Second)})
	if _, ok := f.cached("dev"); ok {
		t.Fatal("an index older than the time told was reused")
	}
	f.store("dev", cachedIndex{at: time.Now().Add(-10 * time.Second)})
	if _, ok := f.cached("dev"); !ok {
		t.Fatal("an index within the time told was fetched again")
	}
}
