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
