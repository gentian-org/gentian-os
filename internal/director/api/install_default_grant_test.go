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

// An install's defaultGrant is read and has no effect: an install that
// carries it, true or false, asks only about installing, writes no grant,
// and the list of the tenant's apps reads none back. So does an install
// for the position user alone, which gives no app.
func TestAnInstallsDefaultGrantHasNoEffect(t *testing.T) {
	h := startWith(t, nil, func(cfg *api.Config) { cfg.Authz = withoutGrant{cfg.Authz} })
	tom := h.token(t, "tenant-demo", "tom")
	for app, body := range map[string]string{
		"wiki": `{"defaultGrant": true}`, "notes": `{"defaultGrant": false}`, "board": ``, "status": `{"positions": ["user"]}`,
	} {
		h.asked.reset()
		if code, answer := h.do(t, "POST", "/v1/tenants/demo/apps/"+app, tom, body); code != http.StatusAccepted {
			t.Fatalf("install %s with body %q: %d %v", app, body, code, answer)
		}
		if got := h.asked.questions(); len(got) != 1 || got[0] != "user:tom can_install_app tenant:demo" {
			t.Fatalf("body %q asked %q, want the one question can_install_app", body, got)
		}
		if appGrantedByDefault(t, h, tom, app) {
			t.Fatalf("body %q installed %s with a default grant", body, app)
		}
	}
	file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if strings.Contains(file, "defaultGrant") || strings.Contains(file, "positions:") {
		t.Fatalf("an install wrote who has the app:\n%s", file)
	}
	subject := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%s", "main")
	if strings.Contains(subject, "everyone") {
		t.Fatalf("the commit speaks of everyone: %s", subject)
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

// Somebody who may install and may not grant is refused an install for a
// position, told why, and nothing is written -- while the same install
// without positions goes through.
func TestAnInstallForPositionsNeedsTheRightToGrantToo(t *testing.T) {
	h := startWith(t, nil, func(cfg *api.Config) { cfg.Authz = withoutGrant{cfg.Authz} })
	tom := h.token(t, "tenant-demo", "tom")

	h.asked.reset()
	before := h.tip(t)
	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{"positions": ["tenant-administrator"]}`)
	if code != http.StatusForbidden {
		t.Fatalf("install for a position without can_grant: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "can_grant") || !strings.Contains(msg, "for a position") {
		t.Fatalf("the refusal does not say what was missing: %v", body)
	}
	// The refusal asks again as tom of his token's realm, the one a role
	// holder's tuple names (authz.CheckAs); nothing else is asked.
	if got := h.asked.questions(); len(got) != 3 || got[1] != "user:tom can_grant tenant:demo" || got[2] != "user:tenant-demo/tom can_grant tenant:demo" {
		t.Fatalf("asked %q, want can_install_app then can_grant", got)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}

	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, `{}`); code != http.StatusAccepted {
		t.Fatalf("the same install without positions: %d %v", code, body)
	}

	// And somebody who may not install is refused before granting is asked.
	h.asked.reset()
	mia := h.token(t, "tenant-demo", "mia")
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/notes", mia, `{"positions": ["tenant-administrator"]}`); code != http.StatusForbidden {
		t.Fatalf("install for a position by a member: %d", code)
	}
	if got := h.asked.questions(); len(got) != 2 || got[0] != "user:mia can_install_app tenant:demo" || got[1] != "user:tenant-demo/mia can_install_app tenant:demo" {
		t.Fatalf("a member's install asked %q, want only can_install_app, as mia and as mia of her realm", got)
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
