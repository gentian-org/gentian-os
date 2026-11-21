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
	"strconv"
	"strings"
	"testing"
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// comingWith is a profile on the cluster that names the apps it comes with.
func comingWith(name string, with ...string) string {
	return dt.ProfileYAML(name) + "  comesWith: [" + strings.Join(with, ", ") + "]\n"
}

// startComingWith is a director whose cluster holds two suites: one that
// comes with the wiki, the board and Nextcloud, which every fixture tenant
// has installed itself, and one that comes with the wiki alone.
func startComingWith(t *testing.T) *harness {
	t.Helper()
	return startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.CataloguePath("suite.yaml"):  comingWith("suite", "wiki", "board", "nextcloud"),
			dt.CataloguePath("reader.yaml"): comingWith("reader", "wiki"),
			dt.CataloguePath("lonely.yaml"): comingWith("lonely", "wiki", "nowhere-to-be-found"),
		})
	})
}

// appEntryOf is one app's entry in a tenant's manifest, "" when it has none.
func appEntryOf(t *testing.T, h *harness, tenant, profile string) string {
	t.Helper()
	text := dt.RemoteFile(t, h.remote, dt.TenantPath(tenant))
	_, rest, found := strings.Cut(text, "- profile: "+profile+"\n")
	if !found {
		if strings.HasSuffix(text, "- profile: "+profile) {
			return "- profile: " + profile
		}
		return ""
	}
	entry := "- profile: " + profile + "\n"
	for _, line := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(line, "    ") {
			break
		}
		entry += line + "\n"
	}
	return entry
}

// Installing an app installs the apps it comes with that the tenant does
// not have, each marked with the app it came with. One the tenant installed
// itself is left exactly as it was.
func TestAnAppIsInstalledWithTheAppsItComesWith(t *testing.T) {
	h := startComingWith(t)
	tom := h.token(t, "tenant-demo", "tom")
	nextcloud := appEntryOf(t, h, "demo", "nextcloud")

	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/suite", tom, "")
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if got := fmt.Sprint(out["installedWith"]); got != "[wiki board]" {
		t.Errorf("installedWith = %s; want the wiki and the board", got)
	}
	for _, name := range []string{"wiki", "board"} {
		if entry := appEntryOf(t, h, "demo", name); !strings.Contains(entry, "cameWith: suite") {
			t.Errorf("%s is not marked as having come with the suite:\n%q", name, entry)
		}
	}
	if entry := appEntryOf(t, h, "demo", "suite"); entry == "" || strings.Contains(entry, "cameWith") {
		t.Errorf("the suite's own entry:\n%q", entry)
	}
	if got := appEntryOf(t, h, "demo", "nextcloud"); got != nextcloud {
		t.Errorf("the tenant's own Nextcloud was changed:\n%q\nwas\n%q", got, nextcloud)
	}

	// Asked again, nothing is installed again.
	before := h.tip(t)
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/suite", tom, ""); code != http.StatusOK || h.tip(t) != before {
		t.Errorf("a second install = %d %v, and the repository moved: %v", code, out, h.tip(t) != before)
	}
}

// The apps an app comes with are written in one commit, and the install is
// finished once begun: a caller that hangs up while it is being written
// still leaves the tenant with the app and everything it came with.
func TestAnInstallWithOtherAppsIsFinishedWithoutItsCaller(t *testing.T) {
	h := startComingWith(t)
	before := dt.Git(t, "", "--git-dir", h.remote, "rev-list", "--count", "main")

	req, err := http.NewRequest("POST", h.URL+"/v1/tenants/demo/apps/suite", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token(t, "tenant-demo", "tom"))
	// Long enough for the request to arrive, far too short for its writes.
	client := &http.Client{Timeout: 150 * time.Millisecond}
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Skip("the install answered before the caller gave up; nothing to show here")
	}

	deadline := time.Now().Add(30 * time.Second)
	for appEntryOf(t, h, "demo", "suite") == "" {
		if time.Now().After(deadline) {
			t.Fatal("the install was not finished after its caller hung up")
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, name := range []string{"wiki", "board"} {
		if entry := appEntryOf(t, h, "demo", name); !strings.Contains(entry, "cameWith: suite") {
			t.Errorf("%s did not come with the suite:\n%q", name, entry)
		}
	}
	after := dt.Git(t, "", "--git-dir", h.remote, "rev-list", "--count", "main")
	if fmt.Sprint(atoi(t, after)-atoi(t, before)) != "2" {
		t.Errorf("the install made %s commits on top of %s; want two, one for what it comes with and one for the app", after, before)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// An app that comes with one nobody offers is not installed, and neither is
// anything else it comes with.
func TestAnAppIsNotInstalledWithoutWhatItComesWith(t *testing.T) {
	h := startComingWith(t)
	before := h.tip(t)
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/lonely", h.token(t, "tenant-demo", "tom"), "")
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "nowhere-to-be-found") {
		t.Fatalf("install = %d %v; want a refusal naming what is missing", code, out)
	}
	if h.tip(t) != before {
		t.Error("a refused install wrote to the repository")
	}
}

// What came with an app is removed with it and not by hand. What the tenant
// installed itself stays.
func TestWhatCameWithAnAppIsRemovedWithIt(t *testing.T) {
	h := startComingWith(t)
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/suite", tom, ""); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}

	before := h.tip(t)
	code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/wiki", tom, "")
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "suite") || h.tip(t) != before {
		t.Fatalf("uninstalling the wiki by hand = %d %v; want a refusal naming the suite", code, out)
	}

	code, out = h.do(t, "DELETE", "/v1/tenants/demo/apps/suite", tom, "")
	if code != http.StatusAccepted || fmt.Sprint(out["removedWith"]) != "[board wiki]" {
		t.Fatalf("uninstall = %d %v; want the board and the wiki removed with it", code, out)
	}
	for _, name := range []string{"suite", "wiki", "board"} {
		if entry := appEntryOf(t, h, "demo", name); entry != "" {
			t.Errorf("%s is still installed:\n%q", name, entry)
		}
	}
	if appEntryOf(t, h, "demo", "nextcloud") == "" {
		t.Error("the tenant's own Nextcloud went with the suite")
	}
}

// An app two installed apps come with stays while one of them does, and is
// then the other's.
func TestWhatAnotherAppComesWithTooStays(t *testing.T) {
	h := startComingWith(t)
	tom := h.token(t, "tenant-demo", "tom")
	for _, name := range []string{"suite", "reader"} {
		if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/"+name, tom, ""); code != http.StatusAccepted {
			t.Fatalf("install %s = %d %v", name, code, out)
		}
	}
	code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/suite", tom, "")
	if code != http.StatusAccepted || fmt.Sprint(out["removedWith"]) != "[board]" {
		t.Fatalf("uninstall = %d %v; want the board alone removed with it", code, out)
	}
	if entry := appEntryOf(t, h, "demo", "wiki"); !strings.Contains(entry, "cameWith: reader") {
		t.Errorf("the wiki is not the reader's now:\n%q", entry)
	}
	code, out = h.do(t, "DELETE", "/v1/tenants/demo/apps/reader", tom, "")
	if code != http.StatusAccepted || fmt.Sprint(out["removedWith"]) != "[wiki]" {
		t.Fatalf("uninstalling the reader = %d %v; want the wiki removed with it", code, out)
	}
}

// A build that comes with an app the build before it did not: the tenant
// moved to it gets that app, marked as having come with the moved one, and
// before the pin moves. A build that comes with an app nobody offers is not
// moved to.
func TestAMoveBringsWhatTheNewBuildComesWith(t *testing.T) {
	one := profileNamed("notes", "one")
	two := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "2.0.0"`) + "  comesWith: [pad]\n"
	three := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "3.0.0"`) + "  comesWith: [pad, nowhere-to-be-found]\n"
	content := served{"gentian": {"notes": one, "pad": profileNamed("pad", "one")}}
	src := catalogues(t, content)
	op := &rolloutsOperator{}
	h := startCatalogues(t, src, withOperator(op))
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes", tom, fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(one))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if entry := appEntryOf(t, h, "demo", "pad"); entry != "" {
		t.Fatalf("the first build brought the pad:\n%q", entry)
	}
	running := func(digest string) []map[string]any {
		return []map[string]any{{"tenant": "demo", "profile": "notes", "pinned": digest, "running": digest, "target": digest, "phase": "Done", "ready": true}}
	}
	op.set(running(sha(one)))
	content["gentian"]["notes"] = two
	srv := h.server()

	if moved := srv.RunLatestMoves(context.Background()); moved != 1 {
		t.Fatalf("the mover moved %d, want the one app", moved)
	}
	if entry := appEntryOf(t, h, "demo", "notes"); !strings.Contains(entry, sha(two)) {
		t.Fatalf("the app was not moved:\n%q", entry)
	}
	if entry := appEntryOf(t, h, "demo", "pad"); !strings.Contains(entry, "cameWith: notes") {
		t.Fatalf("the pad did not come with the moved app:\n%q", entry)
	}

	op.set(running(sha(two)))
	content["gentian"]["notes"] = three
	if moved := srv.RunLatestMoves(context.Background()); moved != 0 {
		t.Fatalf("the mover moved %d to a build whose companion nobody offers", moved)
	}
	if entry := appEntryOf(t, h, "demo", "notes"); !strings.Contains(entry, sha(two)) {
		t.Fatalf("the app left the build it was at:\n%q", entry)
	}
}

// A tenant already at a build that comes with an app it does not have is
// brought that app by the pass beside the mover; an app that came with
// another, and a tenant that has everything, are left as they are.
func TestATenantAtABuildIsBroughtWhatTheBuildComesWith(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.CataloguePath("suite.yaml"): comingWith("suite", "wiki", "board", "nextcloud"),
		})
		tenant := dt.RemoteFile(t, remote, dt.TenantPath("demo"))
		dt.Commit(t, remote, map[string]string{dt.TenantPath("demo"): strings.TrimRight(tenant, "\n") + "\n  - profile: suite\n"})
	})
	nextcloud := appEntryOf(t, h, "demo", "nextcloud")
	srv := h.server()

	if n := srv.RunComesWith(context.Background()); n != 2 {
		t.Fatalf("the pass installed %d apps, want the wiki and the board", n)
	}
	for _, name := range []string{"wiki", "board"} {
		if entry := appEntryOf(t, h, "demo", name); !strings.Contains(entry, "cameWith: suite") {
			t.Errorf("%s is not marked as having come with the suite:\n%q", name, entry)
		}
	}
	if got := appEntryOf(t, h, "demo", "nextcloud"); got != nextcloud {
		t.Errorf("the tenant's own Nextcloud was changed:\n%q\nwas\n%q", got, nextcloud)
	}
	if by := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%b", "main"); !strings.Contains(by, "platform:comes-with") {
		t.Errorf("the pass's commit does not name it:\n%s", by)
	}
	before := h.tip(t)
	if n := srv.RunComesWith(context.Background()); n != 0 || h.tip(t) != before {
		t.Fatalf("a second pass installed %d apps, and the repository moved: %v", n, h.tip(t) != before)
	}
}

// A profile the platform ships for the apps that come with it is in no
// catalog and not in the repository. An app that names it is installed with
// it all the same, the entry naming no catalog and no build: the cluster
// holds the profile, at the build the platform is at.
func TestAnAppBringsAProfileThePlatformShips(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.CataloguePath("agents.yaml"): comingWith("agents", "liaison"),
			// A placed component of the platform is not an app's to bring.
			dt.CataloguePath("grabby.yaml"): comingWith("grabby", "porter"),
		})
	})
	tom := h.token(t, "tenant-demo", "tom")

	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/agents", tom, "")
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if got := fmt.Sprint(out["installedWith"]); got != "[liaison]" {
		t.Errorf("installedWith = %s; want the liaison", got)
	}
	entry := appEntryOf(t, h, "demo", "liaison")
	if !strings.Contains(entry, "cameWith: agents") {
		t.Errorf("the liaison's entry does not say what it came with:\n%s", entry)
	}
	if strings.Contains(entry, "catalogue:") || strings.Contains(entry, "digest:") {
		t.Errorf("the liaison is in no catalog and has no build of its own:\n%s", entry)
	}

	code, out = h.do(t, "POST", "/v1/tenants/demo/apps/grabby", tom, "")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an app that comes with a placed component = %d %v; want it refused", code, out)
	}
	if entry := appEntryOf(t, h, "demo", "porter"); entry != "" {
		t.Errorf("the porter was installed by an app:\n%s", entry)
	}
}
