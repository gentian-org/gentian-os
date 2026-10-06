/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// indexedSource serves profiles and an index.yaml, which is what a catalogue
// source publishes for a cluster to browse it.
func indexedSource(t *testing.T, index string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.yaml":
			_, _ = w.Write([]byte(index))
		case strings.HasSuffix(r.URL.Path, "/profiles/element.yaml"):
			_, _ = w.Write([]byte(elementProfile))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sampleIndex(digest string) string {
	return fmt.Sprintf(`entries:
  - name: element
    version: "1.0.0"
    edition: ce
    trustTier: certified
    digest: %q
  - name: timesheets
    version: "2.1.0"
    edition: pe
    trustTier: experimental
    digest: %q
  - name: activepieces-me
    version: "3.0.0"
    edition: me
    digest: %q
  - name: odoo-ee
    version: "4.0.0"
    edition: ee
    digest: %q
`, digest, digest, digest, digest)
}

// A cluster lists its own catalogues' community and private entries, and
// nothing else: a maintained or licensed entry is the App Store's to describe
// and the cluster only says how many there are (AD-14).
func TestTheClusterListsOnlyItsOwnEditionsAndCountsTheRest(t *testing.T) {
	src := indexedSource(t, sampleIndex(sha(elementProfile)))
	h := startWithCatalogue(t, src, map[string]string{"in-house": src.URL},
		gitops.CatalogueSource{Name: "in-house", URL: src.URL, Tenants: []string{"demo"}})
	tom := h.token(t, "tenant-demo", "tom")

	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/in-house/entries", tom, "")
	if code != http.StatusOK {
		t.Fatalf("entries = %d %v", code, out)
	}
	entries, _ := out["entries"].([]any)
	names := make([]string, 0, len(entries))
	for _, raw := range entries {
		names = append(names, raw.(map[string]any)["coordinate"].(string))
	}
	// Sorted by name, which is why timesheets follows element.
	if strings.Join(names, ",") != "in-house/element,in-house/timesheets" {
		t.Fatalf("listed %v -- me and ee must not be listed", names)
	}
	if out["storeOnly"] != float64(2) {
		t.Fatalf("storeOnly = %v, want the two the store owns", out["storeOnly"])
	}
	if out["storeUrl"] != "https://store.example.com" {
		t.Fatalf("no route to the store: %v", out["storeUrl"])
	}

	// An open source's entries are installable from here, digest and all:
	// the claim named the source and opened it to this tenant.
	first := entries[0].(map[string]any)
	if first["installable"] != true || first["digest"] != sha(elementProfile) {
		t.Fatalf("an open source's entry is not installable: %v", first)
	}
	if first["edition"] != "ce" || first["version"] != "1.0.0" {
		t.Fatalf("entry = %v", first)
	}
	// And nothing a shop would show. A cluster does not pretend to be one.
	for _, key := range []string{"displayName", "summary", "description", "icon", "price"} {
		if _, ok := first[key]; ok {
			t.Fatalf("the cluster's own listing carries %q, which is the store's to say", key)
		}
	}
}

// A source the claim did not open to this tenant is still listed -- with its
// digests, which are no secret -- and offers no install from here: the store
// is the route.
func TestASourceNotOpenToTheTenantOffersNoInstallFromHere(t *testing.T) {
	src := indexedSource(t, sampleIndex(sha(elementProfile)))
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL},
		gitops.CatalogueSource{Name: "main", URL: src.URL})
	tom := h.token(t, "tenant-demo", "tom")

	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/main/entries", tom, "")
	if code != http.StatusOK {
		t.Fatalf("entries = %d %v", code, out)
	}
	if out["open"] != false {
		t.Fatalf("a source opened to nobody reads as open: %v", out["open"])
	}
	for _, raw := range out["entries"].([]any) {
		e := raw.(map[string]any)
		if e["digest"] != sha(elementProfile) {
			t.Fatalf("the entry does not state its build: %v", e)
		}
		if e["installable"] != false {
			t.Fatalf("an entry of a source not open to the tenant offers a local install: %v", e)
		}
	}
}

// The list of catalogues is the other half: which they are, and which of them
// the Cluster claim opened to this tenant.
func TestListingCataloguesSaysWhichAreOpenToThisTenant(t *testing.T) {
	src := indexedSource(t, sampleIndex(sha(elementProfile)))
	h := startWithCatalogue(t, src,
		map[string]string{"main": src.URL, "in-house": src.URL},
		gitops.CatalogueSource{Name: "main", URL: src.URL},
		gitops.CatalogueSource{Name: "in-house", URL: src.URL, Tenants: []string{"demo"}})

	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", h.token(t, "tenant-demo", "tom"), "")
	if code != http.StatusOK {
		t.Fatalf("catalogues = %d %v", code, out)
	}
	got := map[string]bool{}
	for _, raw := range out["catalogues"].([]any) {
		c := raw.(map[string]any)
		got[c["name"].(string)] = c["open"].(bool)
	}
	if len(got) != 2 || got["main"] || !got["in-house"] {
		t.Fatalf("catalogues = %v", got)
	}

	// The same source is open to demo and not to solo: it is per tenant.
	code, out = h.do(t, "GET", "/v1/tenants/solo/catalogues", h.token(t, "tenant-solo", "tina"), "")
	if code != http.StatusOK {
		t.Fatalf("catalogues for solo = %d %v", code, out)
	}
	for _, raw := range out["catalogues"].([]any) {
		if raw.(map[string]any)["open"] == true {
			t.Fatalf("a source opened to demo is open to solo: %v", raw)
		}
	}
}

// A source that publishes bundles but no index cannot be browsed from here.
// That is an empty list, not a broken screen.
func TestASourceWithNoIndexListsNothing(t *testing.T) {
	src := catalogueSource(t, elementProfile) // serves profiles, no index.yaml
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/main/entries",
		h.token(t, "tenant-demo", "tom"), "")
	if code != http.StatusOK {
		t.Fatalf("entries = %d %v", code, out)
	}
	if len(out["entries"].([]any)) != 0 || out["storeOnly"] != float64(0) {
		t.Fatalf("entries = %v", out)
	}
}

// A catalogue this cluster does not name is not browsable, whatever the
// caller asks for.
func TestAnUndeclaredCatalogueIsNotFound(t *testing.T) {
	src := indexedSource(t, sampleIndex(sha(elementProfile)))
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	code, _ := h.do(t, "GET", "/v1/tenants/demo/catalogues/elsewhere/entries",
		h.token(t, "tenant-demo", "tom"), "")
	if code != http.StatusNotFound {
		t.Fatalf("an undeclared catalogue answered %d", code)
	}
}
