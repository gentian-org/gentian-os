/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package catalogue_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

const nextcloudProfile = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: nextcloud-base-ce
spec:
  classes: [app]
  launch: tile
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/nextcloud
      name: nextcloud
      version: "1.0.0"
`

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// source serves what it is told to, which is how a compromised one is
// simulated: the same URL, different bytes.
func source(t *testing.T, body string) served {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/profiles/nextcloud-base-ce.yaml") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	f := catalogue.NewFetcher()
	// Trusts the test server's certificate and nothing else -- and, being
	// another client, connects to its loopback address, which the fetcher's
	// own refuses (address_test.go).
	f.Client = srv.Client()
	return served{f, catalogue.Source{Key: "cluster/main", Name: "main", URL: srv.URL}}
}

// served is a fetcher and the one catalogue the test serves, asked by
// coordinate the way an install asks.
type served struct {
	*catalogue.Fetcher
	src catalogue.Source
}

func (s served) Fetch(ctx context.Context, coordinate, digest string) (*catalogue.Profile, error) {
	_, name, _ := strings.Cut(coordinate, "/")
	return s.Fetcher.Fetch(ctx, s.src, name, digest)
}

func TestAProfileArrivesWhenItMatchesTheDigest(t *testing.T) {
	f := source(t, nextcloudProfile)
	got, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(nextcloudProfile))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "nextcloud-base-ce" {
		t.Fatalf("name = %q", got.Name)
	}
	// What is committed is what was hashed. Re-serialising would produce
	// bytes nobody verified, however equivalent they looked.
	if string(got.Body) != nextcloudProfile {
		t.Fatal("the bundle was not returned byte for byte")
	}
	if got.Digest != digestOf(nextcloudProfile) {
		t.Fatalf("digest = %q", got.Digest)
	}
}

// The property the whole design rests on: the source is not trusted, and a
// source that serves something else fails the install rather than changing
// what gets installed.
func TestASourceServingSomethingElseIsRefused(t *testing.T) {
	tampered := strings.Replace(nextcloudProfile,
		"oci://example.invalid/nextcloud", "oci://attacker.invalid/nextcloud", 1)
	f := source(t, tampered)

	_, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(nextcloudProfile))
	if !errors.Is(err, catalogue.ErrDigestMismatch) {
		t.Fatalf("a tampered bundle was accepted, or refused for the wrong reason: %v", err)
	}
	// The message does not repeat what the source claimed: a number the
	// attacker chose is not evidence.
	if strings.Contains(err.Error(), digestOf(tampered)) {
		t.Error("the refusal quotes the digest the source served")
	}
}

// Even a correct digest does not make something a profile.
func TestTheBundleHasToBeTheProfileItClaims(t *testing.T) {
	for name, body := range map[string]string{
		"another kind": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: nextcloud-base-ce\n",
		"another name": strings.Replace(nextcloudProfile, "nextcloud-base-ce", "something-else", 1),
		"not yaml":     "\x00\x01 this is not a document",
	} {
		f := source(t, body)
		if _, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A source that answers forever does not hold an install open forever.
func TestAnOversizedBundleIsRefused(t *testing.T) {
	huge := nextcloudProfile + strings.Repeat("# padding\n", 200_000)
	f := source(t, huge)
	if _, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(huge)); err == nil {
		t.Fatal("a bundle past the limit was accepted")
	}
}

// A bundle is one profile and what travels with it. Everything in the file is
// applied, so every document in it is looked at before anything is returned
// to be written: a second document that is not a companion of this profile
// is refused whatever it is, and a trailing separator or a comment is not a
// document.
func TestABundleWithADocumentItMayNotHoldIsRefused(t *testing.T) {
	for what, extra := range map[string]string{
		"a ConfigMap that is nobody's": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rides-along\n",
		"a Secret":                     "apiVersion: v1\nkind: Secret\nmetadata:\n  name: nextcloud-base-ce\n",
		"a second profile":             strings.Replace(nextcloudProfile, "name: nextcloud-base-ce", "name: other", 1),
	} {
		body := nextcloudProfile + "---\n" + extra
		f := source(t, body)
		if _, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(body)); !errors.Is(err, profilebundle.ErrRefused) {
			t.Fatalf("%s was not refused: %v", what, err)
		}
	}

	harmless := "# a comment\n---\n" + nextcloudProfile + "\n---\n# nothing here\n"
	f := source(t, harmless)
	p, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(harmless))
	if err != nil {
		t.Fatalf("separators and comments are not documents: %v", err)
	}
	if len(p.Companions) != 0 {
		t.Fatalf("companions of a profile alone: %v", p.Companions)
	}
}

const nextcloudPage = `apiVersion: v1
kind: ConfigMap
metadata:
  name: nextcloud-base-ce.portal-bridge-sso
  labels:
    gentianos.io/profile-name: nextcloud-base-ce
    gentianos.io/asset: portal-bridge-sso
data:
  sso.html: "<html></html>"
`

// The digest is of the file: the profile and its companions together. The
// same profile with another companion is another build, and a catalogue of a
// tenant's own brings the profile alone.
func TestABundleArrivesWithItsCompanionsAtTheDigestOfTheWholeFile(t *testing.T) {
	body := nextcloudProfile + "---\n" + nextcloudPage
	f := source(t, body)
	p, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(body))
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Body) != body || p.Digest != digestOf(body) {
		t.Fatal("what arrived is not the file that was served")
	}
	if len(p.Companions) != 1 || p.Companions[0] != "ConfigMap nextcloud-base-ce.portal-bridge-sso" {
		t.Fatalf("companions: %v", p.Companions)
	}
	// The profile's own digest does not name the bundle.
	if _, err := f.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(nextcloudProfile)); !errors.Is(err, catalogue.ErrDigestMismatch) {
		t.Fatalf("the profile's digest fetched the bundle: %v", err)
	}
	// A companion changed is a mismatch like any other byte.
	changed := source(t, strings.Replace(body, "<html></html>", "<html>!</html>", 1))
	if _, err := changed.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(body)); !errors.Is(err, catalogue.ErrDigestMismatch) {
		t.Fatalf("a changed companion was served at the old digest: %v", err)
	}

	own := source(t, body)
	own.src.Key = profilebundle.TenantOrigin("acme", "own")
	if _, err := own.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(body)); !errors.Is(err, profilebundle.ErrRefused) ||
		!strings.Contains(err.Error(), "only a catalogue of the whole cluster") {
		t.Fatalf("a tenant's own catalogue brought a companion: %v", err)
	}
	alone := source(t, nextcloudProfile)
	alone.src.Key = profilebundle.TenantOrigin("acme", "own")
	if _, err := alone.Fetch(context.Background(), "main/nextcloud-base-ce", digestOf(nextcloudProfile)); err != nil {
		t.Fatalf("a tenant's own catalogue could not bring a profile: %v", err)
	}
}
