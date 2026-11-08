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
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

const aliasRoute = "/v1/clusters/" + dt.Cluster + "/tenants/demo/exposures/nextcloud/shares/alias"

func aliasBody(label string) string {
	return fmt.Sprintf(`{"host":"%s.%s"}`, label, dt.KernelDomain)
}

// The tenant's approver publishes a surface; the cluster's administrator
// gives it a name under the cluster's domain. Neither does the other's part.
func TestTheClustersAdministratorGivesAPublishedSurfaceAnAlias(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom") // perimeter approver of demo
	alice := h.token(t, "gentian", "alice") // platform administrator

	// Not before it is published: an alias publishes nothing.
	if code, out := h.do(t, "PUT", aliasRoute, alice, aliasBody("files")); code != http.StatusConflict {
		t.Fatalf("an alias for an unpublished surface = %d %v, want 409", code, out)
	}
	if code, out := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(30*24*time.Hour))); code != http.StatusAccepted {
		t.Fatalf("publish = %d %v", code, out)
	}

	// The approver may publish, and may not hand out the cluster's names.
	before := h.tip(t)
	if code, out := h.do(t, "PUT", aliasRoute, tom, aliasBody("files")); code != http.StatusForbidden {
		t.Fatalf("the approver set an alias: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused alias moved the repository")
	}

	code, out := h.do(t, "PUT", aliasRoute, alice, aliasBody("files"))
	if code != http.StatusAccepted {
		t.Fatalf("alias = %d %v", code, out)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(trailer, "can_configure cluster:"+dt.Cluster) {
		t.Fatalf("the commit does not name can_configure on the cluster:\n%s", trailer)
	}

	// The approver sees where their surface answers as well.
	code, body := h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %v", code, body)
	}
	live, _ := body["live"].([]any)
	if len(live) != 1 || live[0].(map[string]any)["alias"] != "files" {
		t.Fatalf("live = %v", body["live"])
	}
	found := false
	entries, _ := body["entries"].([]any)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if entry["install"] == "nextcloud" && entry["exposureName"] == "shares" {
			found = entry["alias"] == "files."+dt.KernelDomain
		}
	}
	if !found {
		t.Fatalf("no entry names the alias's address: %v", body["entries"])
	}

	// A review by the approver keeps it.
	if code, out := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(60*24*time.Hour))); code != http.StatusAccepted {
		t.Fatalf("review = %d %v", code, out)
	}
	_, body = h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	live, _ = body["live"].([]any)
	if len(live) != 1 || live[0].(map[string]any)["alias"] != "files" {
		t.Fatalf("after a review, live = %v", body["live"])
	}

	// And the administrator takes it away again.
	if code, out := h.do(t, "DELETE", aliasRoute, alice, ""); code != http.StatusAccepted {
		t.Fatalf("remove = %d %v", code, out)
	}
	if code, out := h.do(t, "DELETE", aliasRoute, alice, ""); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("removing it twice = %d %v", code, out)
	}
}

// What cannot be an alias is refused with the reason, and what is in use is
// a conflict; nothing is committed for either.
func TestAnAliasIsRefusedWithTheReason(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	alice := h.token(t, "gentian", "alice")
	if code, out := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(30*24*time.Hour))); code != http.StatusAccepted {
		t.Fatalf("publish = %d %v", code, out)
	}
	before := h.tip(t)
	for body, want := range map[string]struct {
		code int
		says string
	}{
		`{"host":"files.example.org"}`: {http.StatusUnprocessableEntity, "not a name directly under " + dt.KernelDomain},
		aliasBody("id"):                {http.StatusUnprocessableEntity, "kept for the platform"},
		aliasBody("demo"):              {http.StatusConflict, "tenant demo's own address"},
		`{}`:                           {http.StatusBadRequest, "DELETE removes the alias"},
	} {
		code, out := h.do(t, "PUT", aliasRoute, alice, body)
		if code != want.code || !strings.Contains(fmt.Sprint(out["error"]), want.says) {
			t.Errorf("%s = %d %v, want %d saying %q", body, code, out, want.code, want.says)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused alias moved the repository")
	}
}

// A tenant is not created under a name a surface answers at.
func TestATenantIsNotCreatedUnderAnAliasesName(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	alice := h.token(t, "gentian", "alice")
	if code, out := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(30*24*time.Hour))); code != http.StatusAccepted {
		t.Fatalf("publish = %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", aliasRoute, alice, aliasBody("files")); code != http.StatusAccepted {
		t.Fatalf("alias = %d %v", code, out)
	}
	before := h.tip(t)
	code, out := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants", alice, `{"name":"files"}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "alias of nextcloud/shares of tenant demo") {
		t.Fatalf("a tenant under the alias's name = %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("the refused tenant was committed")
	}
}
