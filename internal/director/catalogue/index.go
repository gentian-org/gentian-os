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

package catalogue

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	// Edition is ce, pe, me or ee. An entry that does not say is taken to be
	// ce, which is what an unmarked public profile has always been.
	Edition gentianov1alpha1.Edition `json:"edition,omitempty"`
	// TrustTier is the profile's own trustTier, repeated here so the index
	// answers without a fetch.
	TrustTier string `json:"trustTier,omitempty"`
	// Digest is the sha256 of the profile bundle, as the SOURCE states it.
	//
	// Which is why it is dropped for an entitled source before anything sees
	// it (see Index): there the digest that governs is the one the store
	// stated over its own TLS, and a source's own number checked against the
	// same source's own bytes is not a check at all (AD-3).
	Digest string `json:"digest,omitempty"`
}

// Index is what a source says it holds.
type Index struct {
	Entries []IndexEntry `json:"entries"`
}

type cachedIndex struct {
	at      time.Time
	entries []IndexEntry
	// dropped counts entries this cluster does not list for itself -- me and
	// ee. Kept as a number so a screen can say how many are in the App Store
	// without listing them, which is the whole shape of AD-14: the store is
	// the route, this is what remains when it is not.
	dropped int
}

// Listing is a source's index as the director serves it.
type Listing struct {
	// Entries are the ce and pe entries, sorted by name.
	Entries []IndexEntry
	// StoreOnly is how many entries were left out because they are me or ee.
	StoreOnly int
}

// Index fetches and caches one source's index, keeping only the editions a
// cluster lists for itself.
//
// entitled says whether the source's entries need a grant from the store. It
// decides one thing here: whether the digest survives into what is served.
func (f *Fetcher) Index(ctx context.Context, catalogue string, entitled bool) (Listing, error) {
	if f == nil {
		return Listing{}, fmt.Errorf("%w: this cluster has no catalogue sources", ErrNotFound)
	}
	base, ok := f.Sources[catalogue]
	if !ok {
		return Listing{}, fmt.Errorf("%w: no source for catalogue %q", ErrNotFound, catalogue)
	}
	if cached, ok := f.cached(catalogue); ok {
		return f.serve(cached, entitled), nil
	}
	fresh, err := f.fetchIndex(ctx, catalogue, base)
	if err != nil {
		return Listing{}, err
	}
	f.store(catalogue, fresh)
	return f.serve(fresh, entitled), nil
}

// serve copies out what a caller may see. The cache holds the source's own
// words; this is where the cluster's rules are applied, so a second caller
// with different rules gets its own answer rather than the first one's.
func (f *Fetcher) serve(c cachedIndex, entitled bool) Listing {
	out := Listing{Entries: make([]IndexEntry, len(c.entries)), StoreOnly: c.dropped}
	copy(out.Entries, c.entries)
	if entitled {
		for i := range out.Entries {
			out.Entries[i].Digest = ""
		}
	}
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

func (f *Fetcher) fetchIndex(ctx context.Context, catalogue, base string) (cachedIndex, error) {
	ref, err := url.Parse(strings.TrimSuffix(base, "/") + "/index.yaml")
	if err != nil {
		return cachedIndex{}, fmt.Errorf("catalogue: source %q is not a URL: %w", base, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.String(), nil)
	if err != nil {
		return cachedIndex{}, err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return cachedIndex{}, fmt.Errorf("catalogue: %s: %w", ref.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// A source that publishes bundles but no index. Not an error worth
		// failing a screen over: it means this catalogue cannot be browsed
		// from here, which is what an empty listing says.
		return cachedIndex{at: time.Now()}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return cachedIndex{}, fmt.Errorf("catalogue: %s answered %d", ref.Host, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndex+1))
	if err != nil {
		return cachedIndex{}, fmt.Errorf("catalogue: reading the index of %s: %w", catalogue, err)
	}
	if len(body) > maxIndex {
		return cachedIndex{}, fmt.Errorf("catalogue: the index of %s is larger than %d bytes", catalogue, maxIndex)
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
		if !e.Edition.Local() {
			out.dropped++
			continue
		}
		if _, err := normaliseDigest(e.Digest); err != nil {
			e.Digest = ""
		}
		out.entries = append(out.entries, e)
	}
	sort.Slice(out.entries, func(i, j int) bool { return out.entries[i].Name < out.entries[j].Name })
	return out, nil
}

// indexCache is embedded in Fetcher.
type indexCache struct {
	mu      sync.Mutex
	indexes map[string]cachedIndex
}
