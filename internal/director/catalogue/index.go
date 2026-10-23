/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package catalogue

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A catalogue source publishes index.yaml beside its profiles/ directory: the
// technical half of a catalogue, and deliberately nothing else.
//
// What it does NOT carry is the point. No display name, no description, no
// icon, no price. Those are the App Store's, they are what the store is for,
// and a cluster that reproduced them would be a second-rate copy of a screen
// somebody else keeps current. What the cluster can answer for itself is the
// narrow question a person asks when the store is not the answer: what is in
// my catalogues, at which version, and may this tenant install it.
//
// So this is the fallback, and it is meant to look like one.

// maxIndex bounds what will be read. An index is one line per entry; a
// catalogue of a thousand apps is well under this.
const maxIndex = 1 << 20

// indexTTL is how long a fetched index is reused. A catalogue changes when
// somebody publishes to it, which is not often, and a store screen that is
// opened, filtered and closed should not be a burst of requests to somebody
// else's web server.
const indexTTL = 5 * time.Minute

// IndexEntry is one entry as its source describes it.
type IndexEntry struct {
	// Name is the profile's metadata.name, which is the second half of its
	// coordinate.
	Name string `json:"name"`
	// Version is the catalogue version of this entry.
	Version string `json:"version,omitempty"`
	// Edition is ce, me, ie, pe or ee. An entry that does not say is taken to be
	// ce, which is what an unmarked public profile has always been.
	Edition gentianov1alpha1.Edition `json:"edition,omitempty"`
	// TrustTier is the profile's own trustTier, repeated here so the index
	// answers without a fetch.
	TrustTier string `json:"trustTier,omitempty"`
	// Digest is the sha256 of the profile bundle, as the SOURCE states it:
	// the build an install from this listing asks for. The trust in it is
	// the Cluster claim naming the source.
	Digest string `json:"digest,omitempty"`
	// Platform is the build's spec.requires.platform, repeated here so a
	// listing can say which entries this cluster cannot install without a
	// fetch. The bundle's own value is what an install is held to.
	Platform string `json:"platform,omitempty"`
}

// Index is what a source says it holds.
type Index struct {
	Entries []IndexEntry `json:"entries"`
}

type cachedIndex struct {
	at      time.Time
	entries []IndexEntry
	// storeOnly holds the entries this cluster does not list for itself --
	// pe and ee. A screen says how many are in the App Store without
	// listing them, which is the whole shape of AD-14: the store is the
	// route, this is what remains when it is not. They are kept all the
	// same, for the one reader that is not a screen: the update report,
	// which compares an app a tenant has already acquired and installed
	// through the store with the build its catalogue offers now.
	storeOnly []IndexEntry
}

// Listing is a source's index as the director serves it.
type Listing struct {
	// Entries are the ce, me and ie entries, sorted by name: what a tenant may
	// install from here by naming it.
	Entries []IndexEntry
	// StoreOnly are the pe and ee entries, sorted by name. They are not
	// listed and not installed from a listing; the store is the route to
	// them. An app that was acquired and installed that way is already the
	// tenant's, and these say what its newest build is.
	StoreOnly []IndexEntry
}

// Index fetches and caches one source's index, keeping only the editions a
// cluster lists for itself.
func (f *Fetcher) Index(ctx context.Context, src Source) (Listing, error) {
	if f == nil {
		return Listing{}, fmt.Errorf("%w: this director fetches from no catalogue", ErrNotFound)
	}
	// By the catalogue and by where it is read from: a catalogue removed and
	// added again at another address, or as another artifact, is another
	// catalogue under the same name.
	key := src.Key + "\n" + src.URL
	if src.OCI != nil {
		key += "\n" + ociKey(src)
	}
	if cached, ok := f.cached(key); ok {
		return serve(cached), nil
	}
	fresh, err := f.fetchIndex(ctx, src)
	if err != nil {
		return Listing{}, err
	}
	f.store(key, fresh)
	return serve(fresh), nil
}

// serve copies the cached index out, so a caller cannot change what the next
// one is given.
func serve(c cachedIndex) Listing {
	out := Listing{Entries: make([]IndexEntry, len(c.entries)), StoreOnly: make([]IndexEntry, len(c.storeOnly))}
	copy(out.Entries, c.entries)
	copy(out.StoreOnly, c.storeOnly)
	return out
}

func (f *Fetcher) cached(catalogue string) (cachedIndex, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.indexes[catalogue]
	if !ok || time.Since(c.at) > indexTTL {
		return cachedIndex{}, false
	}
	return c, true
}

func (f *Fetcher) store(catalogue string, c cachedIndex) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.indexes == nil {
		f.indexes = map[string]cachedIndex{}
	}
	f.indexes[catalogue] = c
}

func (f *Fetcher) fetchIndex(ctx context.Context, src Source) (cachedIndex, error) {
	catalogue := src.Name
	body, err := f.read(ctx, src, "index.yaml", "the index of "+catalogue, maxIndex)
	if errors.Is(err, fs.ErrNotExist) {
		// A source that publishes bundles but no index. Not an error worth
		// failing a screen over: it means this catalogue cannot be browsed
		// from here, which is what an empty listing says.
		return cachedIndex{at: time.Now()}, nil
	}
	if err != nil {
		return cachedIndex{}, err
	}
	var idx Index
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return cachedIndex{}, fmt.Errorf("catalogue: the index of %s does not parse: %w", catalogue, err)
	}

	out := cachedIndex{at: time.Now()}
	seen := map[string]bool{}
	for _, e := range idx.Entries {
		e.Name = strings.TrimSpace(e.Name)
		if e.Name == "" || strings.ContainsAny(e.Name, "/:# \t\r\n") || seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		if e.Edition == "" {
			e.Edition = gentianov1alpha1.EditionCE
		}
		if _, err := normaliseDigest(e.Digest); err != nil {
			e.Digest = ""
		}
		if !e.Edition.Local() {
			out.storeOnly = append(out.storeOnly, e)
			continue
		}
		out.entries = append(out.entries, e)
	}
	byName := func(entries []IndexEntry) func(i, j int) bool {
		return func(i, j int) bool { return entries[i].Name < entries[j].Name }
	}
	sort.Slice(out.entries, byName(out.entries))
	sort.Slice(out.storeOnly, byName(out.storeOnly))
	return out, nil
}

// indexCache is embedded in Fetcher.
type indexCache struct {
	mu      sync.Mutex
	indexes map[string]cachedIndex
}
