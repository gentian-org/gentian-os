/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package catalogue materialises a profile on reference (AD-3).
//
// The cluster holds no catalogue. Thirty-odd profiles do not sit in it
// waiting for somebody to want one; a profile arrives when a tenant installs
// it, at a content digest, and it arrives through the director.
//
// Which makes the digest what pins an install. The REQUEST names an entry and
// the digest of the build it means: the App Store's confirmation carries it,
// and so does the cluster's own listing of a source. The SOURCE serves the
// bytes, and it is not trusted: it is a web server somewhere, possibly a
// customer's own. If what it returns does not hash to the digest requested,
// it is refused and nothing is written. So a compromised source can fail an
// install and cannot change what gets installed.
//
// The digest is not a licence and is not signed. Whether a tenant may have an
// app is not decided here at all: it is decided where the app's artefacts are
// pulled, by the credential the tenant holds for their repository.
package catalogue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// ErrDigestMismatch is what a source serving something other than the build
// requested looks like. It is not a retryable error and it is not a
// formatting complaint: it means the two disagree about what this entry IS.
var ErrDigestMismatch = errors.New("catalogue: the bundle does not match the digest requested")

// ErrNotFound is a source that does not have the entry.
var ErrNotFound = errors.New("catalogue: the source does not serve this entry")

// maxBundle bounds what will be read from a source. A ComponentProfile is a
// few kilobytes; a profile with inline tile art is tens. A megabyte is far
// past anything legitimate and stops an unbounded read from a host the
// platform does not control.
const maxBundle = 1 << 20

// Fetcher reads profile bundles from catalogue sources.
type Fetcher struct {
	// Sources maps a catalogue's slug — the first half of a coordinate — to
	// the base URL its bundles are served from.
	//
	// Configuration, not something a caller supplies: an install that could
	// name its own source would be an install that could name its own
	// profile, and then the digest is checked against a number the same
	// person chose.
	Sources map[string]string
	Client  *http.Client

	// indexCache holds each source's index between fetches. See index.go.
	indexCache
}

// NewFetcher returns a Fetcher with a bounded client.
func NewFetcher(sources map[string]string) *Fetcher {
	return &Fetcher{
		Sources: sources,
		Client: &http.Client{
			Timeout: 30 * time.Second,
			// A catalogue source redirecting somewhere else is a source
			// changing which host serves the bytes. The digest still protects
			// what arrives, but there is no reason to follow it.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Known reports whether a catalogue is one this cluster has a source for.
func (f *Fetcher) Known(catalogue string) bool {
	if f == nil {
		return false
	}
	_, ok := f.Sources[catalogue]
	return ok
}

// Profile is a materialised catalogue entry.
type Profile struct {
	// Name is the ComponentProfile's own metadata.name, read from the bundle
	// rather than taken from the coordinate — and then checked against it.
	Name string
	// Body is the bundle exactly as the source served it, unparsed and
	// unformatted. What is committed is what was hashed: re-serialising it
	// would produce bytes nobody verified.
	Body []byte
	// Digest is what it hashed to: "sha256:<hex>".
	Digest string
}

// Fetch reads one entry from its source and refuses anything that is not
// byte-for-byte what the digest names.
//
// coordinate is "<catalogue>/<name>"; digest is "sha256:<hex>".
func (f *Fetcher) Fetch(ctx context.Context, coordinate, digest string) (*Profile, error) {
	catalogue, name, ok := strings.Cut(coordinate, "/")
	if !ok || catalogue == "" || name == "" {
		return nil, fmt.Errorf("catalogue: %q is not <catalogue>/<name>", coordinate)
	}
	base, ok := f.Sources[catalogue]
	if !ok {
		return nil, fmt.Errorf("%w: no source for catalogue %q", ErrNotFound, catalogue)
	}
	want, err := normaliseDigest(digest)
	if err != nil {
		return nil, err
	}

	// The layout the conversion tool writes and the store ingests:
	// profiles/<name>.yaml beside listings/<name>.yaml.
	ref, err := url.Parse(strings.TrimSuffix(base, "/") + "/profiles/" + url.PathEscape(name) + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("catalogue: source %q is not a URL: %w", base, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalogue: %s: %w", ref.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, coordinate)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalogue: %s answered %d", ref.Host, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBundle+1))
	if err != nil {
		return nil, fmt.Errorf("catalogue: reading %s: %w", coordinate, err)
	}
	if len(body) > maxBundle {
		return nil, fmt.Errorf("catalogue: %s is larger than %d bytes", coordinate, maxBundle)
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != want {
		// Deliberately does not say what it got: a mismatch is a security
		// event, and the number the source chose is not evidence of anything.
		return nil, fmt.Errorf("%w: %s", ErrDigestMismatch, coordinate)
	}

	// It hashes correctly, so it is the build that was asked for. It still
	// has to BE a ComponentProfile of the right name — a digest can be that
	// of a document that installs something else entirely, if whoever stated
	// it was ever confused about which file they hashed.
	var head struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("catalogue: %s does not parse: %w", coordinate, err)
	}
	if head.Kind != "ComponentProfile" {
		return nil, fmt.Errorf("catalogue: %s is a %s, not a ComponentProfile", coordinate, head.Kind)
	}
	if head.Metadata.Name != name {
		return nil, fmt.Errorf("catalogue: %s is named %q in the bundle", coordinate, head.Metadata.Name)
	}
	return &Profile{Name: head.Metadata.Name, Body: body, Digest: "sha256:" + got}, nil
}

// CanonicalDigest returns a digest in the one spelling that is recorded,
// "sha256:<lowercase hex>", or an error if it is not a sha256 digest.
func CanonicalDigest(digest string) (string, error) {
	d, err := normaliseDigest(digest)
	if err != nil {
		return "", err
	}
	return "sha256:" + d, nil
}

// normaliseDigest accepts a digest with or without its prefix and returns the
// bare hex.
func normaliseDigest(digest string) (string, error) {
	d := strings.TrimSpace(strings.ToLower(digest))
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) != 64 {
		return "", fmt.Errorf("catalogue: %q is not a sha256 digest", digest)
	}
	for _, c := range d {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", fmt.Errorf("catalogue: %q is not a sha256 digest", digest)
		}
	}
	return d, nil
}

// ParseSources reads the source map from its configured spelling:
// "<slug>=<url>,<slug>=<url>".
func ParseSources(configured string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(configured, ",") {
		slug, base, ok := strings.Cut(strings.TrimSpace(part), "=")
		slug, base = strings.TrimSpace(slug), strings.TrimSpace(base)
		if !ok || slug == "" || base == "" {
			continue
		}
		// http:// is refused: the digest makes the bytes safe, but a cluster
		// fetching its catalogue in clear is one whose traffic says what it
		// runs.
		if !strings.HasPrefix(base, "https://") {
			continue
		}
		out[slug] = base
	}
	return out
}
