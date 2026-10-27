/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// chatProfile is an app the platform places while the cluster serves models.
func chatProfile() string {
	body := profileNamed("chat", "one")
	body = strings.Replace(body, "  labels:\n", "  annotations:\n    gentianos.io/platform-app: \"true\"\n    gentianos.io/requires-capability: llm\n  labels:\n", 1)
	return "# edition: me\n# placedWith: llm\n" + body
}

// While the cluster serves models every tenant has the app the platform
// places with them, for everyone, and nobody can uninstall it; when the
// cluster stops serving models, no tenant has it. The app is not offered.
func TestThePlatformPlacesTheAppsThatComeWithServingModels(t *testing.T) {
	chat := chatProfile()
	src := catalogues(t, served{"gentian": {"chat": chat, "notes": "# edition: me\n" + profileNamed("notes", "one")}})
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "gentian", URL: src.URL + "/gentian"}) +
				"  llm:\n    enabled: true\n    instances: []\n    providers: []\n",
		})
	}, withFetcher(src), func(c *api.Config) { c.UpdaterOff = true })
	srv := h.server()
	tenants := []string{"demo", "solo", "other"}

	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != len(tenants) || removed != 0 {
		t.Fatalf("placed %d and removed %d, want %d and 0", placed, removed, len(tenants))
	}
	g := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	for _, tenant := range tenants {
		app := appNamed(t, g, tenant, "chat")
		if app.Digest != sha(chat) || !app.DefaultGrant {
			t.Errorf("%s: %+v, want the listed build for everyone", tenant, app)
		}
	}
	// Placed once: a second pass changes nothing.
	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != 0 || removed != 0 {
		t.Fatalf("a second pass placed %d and removed %d", placed, removed)
	}

	// Not offered, and not anybody's to remove.
	tom := h.token(t, "tenant-demo", "tom")
	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/gentian/entries", tom, "")
	if code != http.StatusOK {
		t.Fatalf("listing = %d %v", code, out)
	}
	listed := map[string]bool{}
	for _, raw := range out["entries"].([]any) {
		listed[raw.(map[string]any)["name"].(string)] = true
	}
	if listed["chat"] || !listed["notes"] {
		t.Fatalf("listed %v, want notes and not chat", listed)
	}
	if code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/chat", tom, ""); code != http.StatusConflict {
		t.Fatalf("uninstall = %d %v", code, out)
	}

	// The cluster stops serving models: the app goes from every tenant.
	alice := h.token(t, "gentian", "alice")
	if code, out := h.do(t, "PUT", clusterModels, alice, `{"enabled": false}`); code != http.StatusAccepted {
		t.Fatalf("models off = %d %v", code, out)
	}
	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != 0 || removed != len(tenants) {
		t.Fatalf("placed %d and removed %d, want 0 and %d", placed, removed, len(tenants))
	}
	g = gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	for _, tenant := range tenants {
		apps, err := g.Apps(context.Background(), tenant)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range apps {
			if a.Profile == "chat" {
				t.Errorf("%s still has chat", tenant)
			}
		}
	}
}
