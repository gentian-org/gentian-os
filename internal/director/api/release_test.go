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
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// releaseOperator answers the operator's release route with what a test
// sets, and everything else as the importing operator does.
type releaseOperator struct {
	importingOperator
	mu     sync.Mutex
	status map[string]any
}

func (o *releaseOperator) set(status map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status = status
}

func (o *releaseOperator) Get(ctx context.Context, path string, q url.Values) (int, []byte, error) {
	if path == "/v1/release" {
		o.mu.Lock()
		defer o.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"status": o.status, "platform": map[string]any{"version": "0.5.0", "build": "0.5.0"}})
		return http.StatusOK, raw, nil
	}
	return o.importingOperator.Get(ctx, path, q)
}

// farFromNow is a wall-clock "HH:MM" the given hours from now, so a
// one-minute window there is closed while the test runs.
func farFromNow(hours int) string {
	return time.Now().UTC().Add(time.Duration(hours) * time.Hour).Format("15:04")
}

func TestAClusterAdministratorSetsTheReleaseAndTheCatalogueFollows(t *testing.T) {
	op := &releaseOperator{}
	h := startSeeded(t, op, func(remote string) {
		directortest.Commit(t, remote, map[string]string{directortest.ClaimPath: claimWith(gitops.CatalogueSource{Name: "gentian", URL: gitops.DevelopmentCatalogueURL})})
	}, withOperator(op))
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")

	code, out := h.do(t, "GET", "/v1/clusters/"+directortest.Cluster+"/release", alice, "")
	if code != http.StatusOK || out["spec"].(map[string]any)["version"] != nil || out["windowOpen"] != true {
		t.Fatalf("before: %d %v", code, out)
	}
	// A tenant administrator may not.
	if code, _ := h.do(t, "POST", "/v1/clusters/"+directortest.Cluster+"/actions/set-release", tom, `{"version":"v0.5.1"}`); code != http.StatusForbidden {
		t.Fatalf("a tenant administrator set the release: %d", code)
	}
	// Both, neither, a non-tag: refused before anything is written.
	for _, body := range []string{`{}`, `{"version":"v0.5.1","track":"develop"}`, `{"version":"0.5.1"}`, `{"track":"develop","autoUpgrade":"patch"}`} {
		if code, out := h.do(t, "POST", "/v1/clusters/"+directortest.Cluster+"/actions/set-release", alice, body); code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	code, out = h.do(t, "POST", "/v1/clusters/"+directortest.Cluster+"/actions/set-release", alice, `{"version":"v0.5.1","autoUpgrade":"patch"}`)
	if code != http.StatusAccepted || out["status"] != "updated" {
		t.Fatalf("set-release: %d %v", code, out)
	}
	claim := directortest.RemoteFile(t, h.remote, directortest.ClaimPath)
	if !strings.Contains(claim, "  release:\n    version: v0.5.1\n    autoUpgrade: patch") || !strings.Contains(claim, "url: "+gitops.ReleasedCatalogueURL) || strings.Contains(claim, gitops.DevelopmentCatalogueURL) {
		t.Fatalf("the claim after set-release: %q", claim)
	}
	// The same again changes nothing; a version alone keeps the mode.
	if code, out := h.do(t, "POST", "/v1/clusters/"+directortest.Cluster+"/actions/set-release", alice, `{"version":"v0.5.1"}`); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("again: %d %v", code, out)
	}
	op.set(map[string]any{"state": "Applied", "revision": "v0.5.1", "pending": "v0.6.0"})
	code, out = h.do(t, "GET", "/v1/clusters/"+directortest.Cluster+"/release", alice, "")
	spec, status := out["spec"].(map[string]any), out["status"].(map[string]any)
	if code != http.StatusOK || spec["version"] != "v0.5.1" || spec["autoUpgrade"] != "patch" || status["pending"] != "v0.6.0" || out["platform"].(map[string]any)["build"] != "0.5.0" {
		t.Fatalf("after: %d %v", code, out)
	}
	// Back to a track: the mode goes, the catalog comes back.
	if code, _ := h.do(t, "POST", "/v1/clusters/"+directortest.Cluster+"/actions/set-release", alice, `{"track":"develop"}`); code != http.StatusAccepted {
		t.Fatalf("track: %d", code)
	}
	claim = directortest.RemoteFile(t, h.remote, directortest.ClaimPath)
	if !strings.Contains(claim, "  release:\n    track: develop") || strings.Contains(claim, "autoUpgrade") || !strings.Contains(claim, "url: "+gitops.DevelopmentCatalogueURL) {
		t.Fatalf("the claim after the track:\n%s", claim)
	}
}

func TestTheMoverTakesWhatTheModeAllowsInsideTheWindow(t *testing.T) {
	op := &releaseOperator{}
	h := startSeeded(t, op, func(remote string) {
		directortest.Commit(t, remote, map[string]string{directortest.ClaimPath: claimWith(gitops.CatalogueSource{Name: "gentian", URL: gitops.DevelopmentCatalogueURL})})
	}, withOperator(op))
	alice := h.token(t, "gentian", "alice")
	if code, _ := h.do(t, "POST", "/v1/clusters/"+directortest.Cluster+"/actions/set-release", alice, `{"version":"v0.5.1","autoUpgrade":"patch"}`); code != http.StatusAccepted {
		t.Fatalf("set-release: %d", code)
	}
	ctx := context.Background()
	// Nothing available: nothing moves.
	op.set(map[string]any{"state": "Applied", "revision": "v0.5.1", "pending": "v0.6.0"})
	if h.server().RunReleaseMove(ctx) {
		t.Fatal("moved with nothing available")
	}
	// The operator says v0.5.2 may be taken: one commit, the mode kept.
	op.set(map[string]any{"state": "Applied", "revision": "v0.5.1", "available": "v0.5.2", "pending": "v0.6.0"})
	if !h.server().RunReleaseMove(ctx) {
		t.Fatal("did not move to the available release")
	}
	claim := directortest.RemoteFile(t, h.remote, directortest.ClaimPath)
	if !strings.Contains(claim, "    version: v0.5.2\n    autoUpgrade: patch") {
		t.Fatalf("the claim after the mover: %q", claim)
	}
	log := directortest.Git(t, directortest.Clone(t, h.remote), "log", "-1", "--format=%s")
	if !strings.Contains(log, "release v0.5.2") || !strings.Contains(log, "policy:autoUpgrade") {
		t.Fatalf("the mover's commit: %q", log)
	}
	// Already there: nothing more.
	if h.server().RunReleaseMove(ctx) {
		t.Fatal("moved twice")
	}
	// Under none, nothing moves whatever is available.
	if code, _ := h.do(t, "POST", "/v1/clusters/"+directortest.Cluster+"/actions/set-release", alice, `{"version":"v0.5.2","autoUpgrade":"none"}`); code != http.StatusAccepted {
		t.Fatalf("none: %d", code)
	}
	op.set(map[string]any{"state": "Applied", "revision": "v0.5.2", "available": "v0.5.3"})
	if h.server().RunReleaseMove(ctx) {
		t.Fatal("moved under none")
	}
}

func TestTheMoverWaitsForTheWindow(t *testing.T) {
	op := &releaseOperator{}
	// A window that is never open now: one minute long, twelve hours away.
	h := startSeeded(t, op, func(remote string) {
		claim := claimWith(gitops.CatalogueSource{Name: "gentian", URL: gitops.DevelopmentCatalogueURL})
		claim += "    updates:\n      window:\n        start: \"" + farFromNow(12) + "\"\n        end: \"" + farFromNow(12) + "\"\n        timezone: UTC\n  release:\n    version: v0.5.1\n    autoUpgrade: minor\n"
		directortest.Commit(t, remote, map[string]string{directortest.ClaimPath: claim})
	}, withOperator(op))
	op.set(map[string]any{"state": "Applied", "revision": "v0.5.1", "available": "v0.6.0"})
	if h.server().RunReleaseMove(context.Background()) {
		t.Fatal("moved outside the window")
	}
	alice := h.token(t, "gentian", "alice")
	code, out := h.do(t, "GET", "/v1/clusters/"+directortest.Cluster+"/release", alice, "")
	if code != http.StatusOK || out["windowOpen"] != false || out["status"].(map[string]any)["available"] != "v0.6.0" {
		t.Fatalf("view: %d %v", code, out)
	}
}
