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
)

// A build a tenant ran is kept on the cluster, so a tenant can be moved back
// to it after the catalog has moved on and no longer serves it. A build the
// cluster never had is still fetched, and refused when the source does not
// serve it.
func TestABuildTheClusterKeepsIsInstalledWithoutTheSourceServingIt(t *testing.T) {
	one := profileNamed("notes", "one")
	two := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "2.0.0"`)
	content := served{"gentian": {"notes": one}}
	src := catalogues(t, content)
	h := startCatalogues(t, src)
	// The cluster's administrator: under the default mode, latest, a move
	// of an installed app is theirs (updates.go).
	alice := h.token(t, "gentian", "alice")

	install := func(bundle string) (int, map[string]any) {
		return h.do(t, "POST", "/v1/tenants/demo/apps/notes", alice,
			fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(bundle)))
	}
	if code, out := install(one); code != http.StatusAccepted {
		t.Fatalf("install of build one = %d %v", code, out)
	}
	// The catalog moves on: it serves two and not one any more.
	content["gentian"]["notes"] = two
	if code, out := install(two); code != http.StatusAccepted || out["status"] != "updated" {
		t.Fatalf("move to build two = %d %v", code, out)
	}
	// Back to one, which the source no longer serves: the cluster has it.
	code, out := install(one)
	if code != http.StatusAccepted || out["status"] != "updated" {
		t.Fatalf("move back to build one = %d %v", code, out)
	}
	// A build nobody ever installed here is asked of the source, which does
	// not serve it.
	three := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "3.0.0"`)
	if code, out := install(three); code != http.StatusBadGateway {
		t.Fatalf("install of an unknown build = %d %v, want the source's refusal", code, out)
	}
}
