/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"net/http"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// The App Store's terms: recorded on the tenant by whoever may install apps
// in it, read back by whoever sees it, and nobody else's to record.
func TestTheStoreTermsAreRecordedByWhoeverMayInstall(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")   // administers demo
	tina := h.token(t, "tenant-solo", "tina") // administers another tenant
	route := "/v1/tenants/demo/store-terms"

	if code, out := h.do(t, "GET", route, tom, ""); code != http.StatusOK || len(out) != 0 {
		t.Fatalf("before: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", route, tom, `{"version":"2026-10"}`); code != http.StatusAccepted {
		t.Fatalf("accept: %d %v", code, out)
	}
	if doc := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(doc, "  storeTerms:\n    version: \"2026-10\"\n    acceptedAt: ") {
		t.Fatalf("the acceptance is not in git:\n%s", doc)
	}
	if code, out := h.do(t, "GET", route, tom, ""); code != http.StatusOK || out["version"] != "2026-10" || out["acceptedAt"] == "" {
		t.Fatalf("read back: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", route, tom, `{"version":"2026-10"}`); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("the same version again: %d %v", code, out)
	}
	if code, _ := h.do(t, "PUT", route, tom, `{"version":"not one"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid version was accepted: %d", code)
	}
	if code, _ := h.do(t, "PUT", route, tom, `{}`); code != http.StatusBadRequest {
		t.Fatalf("an empty body was accepted: %d", code)
	}
	if code, _ := h.do(t, "PUT", route, tina, `{"version":"2027-01"}`); code != http.StatusForbidden {
		t.Fatalf("another tenant's administrator recorded it: %d", code)
	}
}
