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
func source(t *testing.T, body string) *catalogue.Fetcher {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/profiles/nextcloud-base-ce.yaml") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	f := catalogue.NewFetcher(map[string]string{"main": srv.URL})
	f.Client = srv.Client() // trusts the test server's certificate and nothing else
	return f
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

// A catalogue this cluster has no source for is not fetched from somewhere
// guessed.
func TestAnUnknownCatalogueIsNotFetched(t *testing.T) {
	f := source(t, nextcloudProfile)
	_, err := f.Fetch(context.Background(), "somebody-elses/nextcloud-base-ce", digestOf(nextcloudProfile))
	if !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if f.Known("somebody-elses") {
		t.Fatal("an unconfigured catalogue is reported as known")
	}
}

// Sources are configuration. http:// is refused — the digest protects the
// bytes, but a cluster fetching its catalogue in clear announces what it runs.
func TestSourcesMustBeHTTPS(t *testing.T) {
	got := catalogue.ParseSources("main=https://a.example/c, other=http://b.example/c, broken, empty=")
	if len(got) != 1 || got["main"] != "https://a.example/c" {
		t.Fatalf("parsed = %v", got)
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
