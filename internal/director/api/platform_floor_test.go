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
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/platformversion"
)

// profileWithFloor is a profile that declares the platform line it needs.
func profileWithFloor(name, floor string) string {
	return profileNamed(name, name) + "  requires:\n    platform: \"" + floor + "\"\n"
}

// A build whose floor is above this platform's line is listed with the
// reason and is not installable; asked for anyway, it is refused before
// anything is written. One whose floor this platform meets installs.
func TestABuildAboveThisPlatformsLineIsListedAsUnmetAndRefused(t *testing.T) {
	tooNew := profileWithFloor("future", ">=99.0")
	fine := profileWithFloor("present", ">="+platformversion.Line)
	src := catalogues(t, served{"gentian": {"future": tooNew, "present": fine}})
	h := startCatalogues(t, src)
	tom := h.token(t, "tenant-demo", "tom")

	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/gentian/entries", tom, "")
	if code != http.StatusOK {
		t.Fatalf("entries = %d %v", code, out)
	}
	byName := map[string]map[string]any{}
	for _, raw := range out["entries"].([]any) {
		e := raw.(map[string]any)
		byName[e["name"].(string)] = e
	}
	if e := byName["future"]; e["installable"] != false || e["platform"] != ">=99.0" ||
		e["unmet"] != "needs platform 99.0; this cluster is "+platformversion.Line {
		t.Fatalf("future: %v", e)
	}
	if e := byName["present"]; e["installable"] != true || e["unmet"] != nil {
		t.Fatalf("present: %v", e)
	}

	code, out = h.do(t, "POST", "/v1/tenants/demo/apps/future", tom,
		fmt.Sprintf(`{"coordinate":"gentian/future","digest":%q}`, sha(tooNew)))
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "needs platform 99.0") {
		t.Fatalf("install of a build above this line = %d %v", code, out)
	}
	if catalogueHas(t, h, "future.yaml") {
		t.Fatal("a refused build was materialized")
	}

	code, out = h.do(t, "POST", "/v1/tenants/demo/apps/present", tom,
		fmt.Sprintf(`{"coordinate":"gentian/present","digest":%q}`, sha(fine)))
	if code != http.StatusAccepted {
		t.Fatalf("install of a build this line meets = %d %v", code, out)
	}
}
