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
	"github.com/gentian-org/gentian-os/internal/director/authz"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// An install for everyone is one request and one commit: the grant is a key
// of the entry the install writes, and the list of the tenant's apps reads
// it back.
func TestAnInstallForEveryoneIsCommittedWithTheInstall(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	h.asked.reset()
	before := h.tip(t)
	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"defaultGrant": true}`)
	if code != http.StatusAccepted || body["status"] != "installed" {
		t.Fatalf("install for everyone: %d %v", code, body)
	}
	// Granting access to people is asked as well as installing, and in that
	// order: the route's question, then the one the body raised.
	got := h.asked.questions()
	want := []string{"user:tom can_install_app tenant:demo", "user:tom can_grant tenant:demo"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("an install for everyone asked %q, want %q", got, want)
	}
	if n := dt.Git(t, "", "--git-dir", h.remote, "rev-list", "--count", before+"..main"); n != "1" {
		t.Fatalf("the install and its grant took %s commits, want one", n)
	}
	if file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(file, "  - profile: wiki\n    defaultGrant: true\n") {
		t.Fatalf("the grant is not on the entry in git:\n%s", file)
	}
	// Both decisions are what the commit says allowed it.
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(trailer, "user:tom can_install_app tenant:demo and can_grant tenant:demo allowed") {
		t.Fatalf("the commit does not record both decisions: %s", trailer)
	}

	if !appGrantedByDefault(t, h, tom, "wiki") {
		t.Fatal("the list of apps does not say wiki is for everyone")
	}

	// The same again changes nothing.
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"defaultGrant": true}`); code != http.StatusOK || body["status"] != "already_installed" {
		t.Fatalf("a repeated install for everyone: %d %v", code, body)
	}
	// A body that does not mention the grant leaves it alone...
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{}`); code != http.StatusOK || body["status"] != "already_installed" {
		t.Fatalf("an install that states no grant: %d %v", code, body)
	}
	if !appGrantedByDefault(t, h, tom, "wiki") {
		t.Fatal("an install that stated no grant removed it")
	}
	// ...and one that states the other value updates the entry.
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"defaultGrant": false}`); code != http.StatusAccepted || body["status"] != "updated" {
		t.Fatalf("re-installing per person: %d %v", code, body)
	}
	if appGrantedByDefault(t, h, tom, "wiki") {
		t.Fatal("the grant is still read back after it was withdrawn")
	}
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"defaultGrant": true}`); code != http.StatusAccepted || body["status"] != "updated" {
		t.Fatalf("re-installing for everyone: %d %v", code, body)
	}
	if !appGrantedByDefault(t, h, tom, "wiki") {
		t.Fatal("the grant is not read back after re-installing for everyone")
	}
}

// Only a request that grants is asked about granting. Without the field, and
// with it false, an install asks the one question it always did.
func TestAnInstallThatGrantsNothingAsksOnlyAboutInstalling(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	for app, body := range map[string]string{"wiki": ``, "notes": `{}`, "board": `{"defaultGrant": false}`} {
		h.asked.reset()
		if code, answer := h.do(t, "POST", "/v1/tenants/demo/apps/"+app, tom, body); code != http.StatusAccepted {
			t.Fatalf("install %s with body %q: %d %v", app, body, code, answer)
		}
		if got := h.asked.questions(); len(got) != 1 || got[0] != "user:tom can_install_app tenant:demo" {
			t.Fatalf("body %q asked %q, want the one question can_install_app", body, got)
		}
		if appGrantedByDefault(t, h, tom, app) {
			t.Fatalf("body %q installed %s for everyone", body, app)
		}
	}
}

// withoutGrant answers as the decision point does, except that nobody may
// grant. Model v1 gives a tenant's administrator both relations, so the case
// of holding one and not the other is made here rather than found in the
// fixture; the route must ask about each whatever the model says today.
type withoutGrant struct{ authz.Checker }

func (c withoutGrant) Check(ctx context.Context, id, user, relation, object string) (bool, error) {
	ok, err := c.Checker.Check(ctx, id, user, relation, object)
	if relation == "can_grant" {
		return false, err
	}
	return ok, err
}

// Somebody who may install and may not grant is refused an install for
// everyone, told why, and nothing is written -- while the same install
// without the grant goes through.
func TestAnInstallForEveryoneNeedsTheRightToGrantToo(t *testing.T) {
	h := startWith(t, nil, func(cfg *api.Config) { cfg.Authz = withoutGrant{cfg.Authz} })
	tom := h.token(t, "tenant-demo", "tom")

	h.asked.reset()
	before := h.tip(t)
	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"defaultGrant": true}`)
	if code != http.StatusForbidden {
		t.Fatalf("install for everyone without can_grant: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "can_grant") || !strings.Contains(msg, "for everyone") {
		t.Fatalf("the refusal does not say what was missing: %v", body)
	}
	if got := h.asked.questions(); len(got) != 2 || got[1] != "user:tom can_grant tenant:demo" {
		t.Fatalf("asked %q, want can_install_app then can_grant", got)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}

	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{}`); code != http.StatusAccepted {
		t.Fatalf("the same install without the grant: %d %v", code, body)
	}

	// And somebody who may not install is refused before granting is asked.
	h.asked.reset()
	mia := h.token(t, "tenant-demo", "mia")
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/notes", mia, `{"defaultGrant": true}`); code != http.StatusForbidden {
		t.Fatalf("install for everyone by a member: %d", code)
	}
	if got := h.asked.questions(); len(got) != 1 {
		t.Fatalf("a member's install asked %q, want only can_install_app", got)
	}
}

func appGrantedByDefault(t *testing.T, h *harness, token, profile string) bool {
	t.Helper()
	code, body := h.do(t, "GET", "/v1/tenants/demo/apps", token, "")
	if code != http.StatusOK {
		t.Fatalf("list apps: %d %v", code, body)
	}
	apps, _ := body["apps"].([]any)
	for _, a := range apps {
		app, _ := a.(map[string]any)
		if app["profile"] == profile {
			granted, _ := app["defaultGrant"].(bool)
			return granted
		}
	}
	t.Fatalf("%s is not in the list of apps: %v", profile, apps)
	return false
}
