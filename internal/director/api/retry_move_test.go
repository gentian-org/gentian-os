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
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// longAgo is a movedAt no request of a test can write: the record of a move
// asked for before the test's own requests.
const longAgo = "2026-01-01T00:00:00Z"

var movedAtLine = regexp.MustCompile(`(?m)^(\s*)movedAt: .*$`)

// backdateMoves rewrites every movedAt of a tenant's manifest to longAgo, as
// though the move had been asked for long before: a retry in the same second
// as the install would otherwise write the same stamp.
func backdateMoves(t *testing.T, remote, tenant string) {
	t.Helper()
	text := dt.RemoteFile(t, remote, dt.TenantPath(tenant))
	dt.Commit(t, remote, map[string]string{dt.TenantPath(tenant): movedAtLine.ReplaceAllString(text, "${1}movedAt: "+longAgo)})
}

// An app installed again at the build it is pinned to, whose move to that
// build the platform rolled back or could not start, has the move asked for
// once more: a commit with a fresh movedAt, and an answer that says so. An
// app that runs its pinned build is left alone, and nothing is committed.
func TestInstallingThePinnedBuildAgainRetriesAMoveThatEndedBadly(t *testing.T) {
	one := profileNamed("notes", "one")
	two := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "2.0.0"`)
	content := served{"gentian": {"notes": one}}
	src := catalogues(t, content)
	op := &rolloutsOperator{}
	h := startCatalogues(t, src, withOperator(op))
	tom, alice := h.token(t, "tenant-demo", "tom"), h.token(t, "gentian", "alice")
	install := func(token, bundle string) (int, map[string]any) {
		return h.do(t, "POST", "/v1/tenants/demo/apps/notes", token,
			fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(bundle)))
	}
	if code, out := install(tom, one); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	content["gentian"]["notes"] = two
	if code, out := install(alice, two); code != http.StatusAccepted || out["status"] != "updated" {
		t.Fatalf("move = %d %v", code, out)
	}
	g := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})

	// Current: the pinned build runs, and installing it again changes nothing.
	op.set([]map[string]any{{"tenant": "demo", "profile": "notes", "pinned": sha(two), "running": sha(two), "target": sha(two), "phase": "Done", "ready": true}})
	if code, out := install(alice, two); code != http.StatusOK || out["status"] != "already_installed" {
		t.Fatalf("a current app installed again: %d %v", code, out)
	}

	for _, c := range []struct{ phase, reason, says string }{
		{"RolledBack", "the build did not come up within 15m0s", "retrying the move that was rolled back: the build did not come up within 15m0s"},
		{"Failed", "the export before the move failed: disk full", "retrying the move that did not start: the export before the move failed: disk full"},
	} {
		backdateMoves(t, h.remote, "demo")
		op.set([]map[string]any{{"tenant": "demo", "profile": "notes", "pinned": sha(two), "running": sha(one), "target": sha(two),
			"phase": c.phase, "message": c.reason, "ready": true}})

		// A retry is a move: under latest, not the tenant administrator's.
		if code, out := install(tom, two); code != http.StatusForbidden || !strings.Contains(fmt.Sprint(out["error"]), "latest mode") {
			t.Fatalf("%s: a tenant administrator retried a move in latest mode: %d %v", c.phase, code, out)
		}
		code, out := install(alice, two)
		if code != http.StatusAccepted || out["status"] != "retried" || out["message"] != c.says || out["commit"] == "" {
			t.Fatalf("%s: the retry = %d %v", c.phase, code, out)
		}
		notes := appNamed(t, g, "demo", "notes")
		if notes.Digest != sha(two) || notes.MovedAt == "" || notes.MovedAt == longAgo || !strings.HasPrefix(notes.MovedBy, "alice") {
			t.Fatalf("%s: the manifest after the retry: %+v", c.phase, notes)
		}
		if log := dt.Git(t, dt.Clone(t, h.remote), "log", "-1", "--format=%s"); !strings.Contains(log, "retry the move of notes to sha256:") {
			t.Fatalf("%s: the commit says %q", c.phase, log)
		}
	}
}

// move-outdated asks again for every move to a pinned build that was rolled
// back or did not start, beside the moves it makes, and leaves one under way
// where it is. The platform's own mover does not: a build that keeps failing
// is retried only when somebody asks.
func TestMoveOutdatedRetriesAMoveThatWasRolledBack(t *testing.T) {
	one := profileNamed("notes", "one")
	content := served{"gentian": {"notes": one}}
	src := catalogues(t, content)
	op := &rolloutsOperator{}
	h := startCatalogues(t, src, withOperator(op))
	tom, tina, alice := h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina"), h.token(t, "gentian", "alice")
	for tenant, token := range map[string]string{"demo": tom, "solo": tina} {
		if code, out := h.do(t, "POST", "/v1/tenants/"+tenant+"/apps/notes", token, fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(one))); code != http.StatusAccepted {
			t.Fatalf("%s: install = %d %v", tenant, code, out)
		}
	}
	// A first install records no move, so a retry is the first stamp.
	op.set([]map[string]any{
		{"tenant": "demo", "profile": "notes", "pinned": sha(one), "running": "sha256:" + strings.Repeat("0", 64), "target": sha(one), "phase": "RolledBack", "message": "the verification command failed", "ready": true},
		{"tenant": "solo", "profile": "notes", "pinned": sha(one), "running": "sha256:" + strings.Repeat("0", 64), "target": sha(one), "phase": "Rolling", "ready": false},
	})
	g := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})

	// The platform's mover leaves the rolled-back move alone.
	if moved := h.server().RunLatestMoves(context.Background()); moved != 0 {
		t.Fatalf("the platform's mover moved %d", moved)
	}
	if at := appNamed(t, g, "demo", "notes").MovedAt; at != "" {
		t.Fatalf("the platform's mover retried a rolled-back move: movedAt %s", at)
	}

	code, out := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/actions/move-outdated", alice, "")
	if code != http.StatusAccepted {
		t.Fatalf("move-outdated = %d %v", code, out)
	}
	moved := out["moved"].([]any)
	if len(moved) != 1 {
		t.Fatalf("moved = %v", moved)
	}
	m := moved[0].(map[string]any)
	if m["tenant"] != "demo" || m["status"] != "retried" || m["to"] != sha(one) ||
		m["message"] != "retrying the move that was rolled back: the verification command failed" || m["detail"] == "" {
		t.Fatalf("the retry = %v", m)
	}
	if at := appNamed(t, g, "demo", "notes").MovedAt; at == "" {
		t.Fatalf("demo's movedAt after the retry: %q", at)
	}
	if at := appNamed(t, g, "solo", "notes").MovedAt; at != "" {
		t.Fatalf("a move under way was asked for again: movedAt %s", at)
	}
}
