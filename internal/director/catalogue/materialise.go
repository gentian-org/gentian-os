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
// What arrives is a bundle: one file holding the profile and, after it, the
// few other objects the app needs on a cluster (profilebundle/bundle.go says
// which). One file, one digest over all of it.
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
	"net"
	"net/http"
	"net/url"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// ErrDigestMismatch is what a source serving something other than the build
// requested looks like. It is not a retryable error and it is not a
// formatting complaint: it means the two disagree about what this entry IS.
var ErrDigestMismatch = errors.New("catalogue: the bundle does not match the digest requested")

// ErrNotFound is a source that does not have the entry.
var ErrNotFound = errors.New("catalogue: the source does not serve this entry")

// maxBundle bounds what will be read from a source. A ComponentProfile is a
// few kilobytes; a profile with inline tile art or its own Composition is tens. A megabyte is far
// past anything legitimate and stops an unbounded read from a host the
// platform does not control.
const maxBundle = 1 << 20

// Source is one catalogue as an install or a listing names it: resolved, by
// whoever asked, from what the cluster and the tenant declare.
//
// Which address a name means is never the caller's to supply. An install
// that could name its own source would be an install that could name its own
// profile, and then the digest is checked against a number the same person
// chose. The name comes from the request; the address comes from the Cluster
// claim or the tenant's manifest in git.
type Source struct {
	// Key tells this catalogue from every other on the cluster:
	// "cluster/<name>" or "tenant/<tenant>/<name>". Two tenants may each have
	// a catalogue called the same thing; they are not the same catalogue, and
	// what is remembered of one is never served for the other.
	Key string
	// Name is the catalogue's name: the first half of a coordinate.
	Name string
	// URL is the base address its index and profiles are served from.
	URL string
}

// Fetcher reads profile bundles and indexes from catalogue sources.
type Fetcher struct {
	// Client makes the requests. NewFetcher's refuses every address that is
	// not a public https one, at the moment it connects (address.go).
	Client *http.Client
	// Vet checks an address when a catalogue is added: as it is written, and
	// what its host resolves to.
	Vet func(ctx context.Context, address string) error
	// indexCache holds each source's index between fetches. See index.go.
	indexCache
}

// NewFetcher returns a Fetcher that fetches from public https addresses and
// from nothing else.
func NewFetcher() *Fetcher {
	return &Fetcher{
		Client: guardedClient(),
		Vet: func(ctx context.Context, address string) error {
			return vet(ctx, net.DefaultResolver, address)
		},
	}
}

// Profile is a materialised catalogue entry.
type Profile struct {
	// Name is the ComponentProfile's own metadata.name, read from the bundle
	// rather than taken from the coordinate — and then checked against it.
	Name string
	// Body is the bundle exactly as the source served it -- the profile and
	// whatever travels with it, one file -- unparsed and unformatted. What is committed is what was hashed: re-serialising it
	// would produce bytes nobody verified.
	Body []byte
	// Digest is what it hashed to: "sha256:<hex>".
	Digest string
	// Companions names what the bundle holds beside the profile, each as
	// "<Kind> <name>". Empty for a profile that travels alone.
	Companions []string
	// Definition is the profile as the cluster would read it once applied:
	// what is asked before the bundle is committed, such as which addresses
	// it would take. Read from Body; Body is what is committed.
	Definition *gentianov1alpha1.ComponentProfile
}

// Fetch reads one entry from its source and refuses anything that is not
// byte-for-byte what the digest names.
//
// name is the entry's name, the second half of its coordinate; digest is
// "sha256:<hex>".
func (f *Fetcher) Fetch(ctx context.Context, src Source, name, digest string) (*Profile, error) {
	coordinate := src.Name + "/" + name
	if src.Name == "" || name == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("catalogue: %q is not <catalogue>/<name>", coordinate)
	}
	base := src.URL
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
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("catalogue: %s redirects elsewhere, and a redirect is not followed: "+
			"declare the address the catalogue is served from", ref.Host)
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
	// has to BE a bundle -- a digest can be that of a file that installs
	// something else entirely, if whoever stated it was ever confused about
	// which file they hashed. The file is committed whole and everything in
	// it is applied, so everything in it is looked at: one ComponentProfile
	// of this name, first, and after it only what this profile may bring
	// from a catalogue of this origin (profilebundle.Check). Before anything
	// is written, and again by the operator before anything is rolled out.
	bundle, err := profilebundle.Check(body, name, src.Key)
	if err != nil {
		return nil, fmt.Errorf("catalogue: %s: %w", coordinate, err)
	}
	companions := make([]string, 0, len(bundle.Companions))
	for _, c := range bundle.Companions {
		companions = append(companions, c.String())
	}
	// And read as the cluster will read it. A profile that does not read as
	// one is refused here rather than committed and found out at rollout:
	// nothing could be said of what it would do.
	definition, err := profilebundle.ReadProfile(body)
	if err != nil {
		return nil, fmt.Errorf("catalogue: %s: %w: its profile cannot be read as a ComponentProfile: %v",
			coordinate, profilebundle.ErrRefused, err)
	}
	return &Profile{Name: bundle.Name, Body: body, Digest: "sha256:" + got, Companions: companions, Definition: definition}, nil
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
