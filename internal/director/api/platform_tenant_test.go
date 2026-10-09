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
	"os"
	"path/filepath"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// adoptKernelRealm rewrites a fixture tenant's manifest into one that adopts
// the kernel realm, which is what makes a tenant the platform tenant: the
// field, whatever the tenant is called. Whoever could install apps in it
// before still holds can_install_app on it afterwards, so a refusal that
// follows is the tenant's and not the caller's.
func adoptKernelRealm(t *testing.T, h *harness, tenant string, more ...string) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	dt.Git(t, "", "clone", h.remote, seed)
	manifest := "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: " + tenant +
		"\nspec:\n  displayName: Platform\n  isolation:\n    mode: namespace\n    keycloakRealm: kernel\n  apps:\n  - profile: nextcloud\n"
	for _, app := range more {
		manifest += "  - profile: " + app + "\n"
	}
	if err := os.WriteFile(filepath.Join(seed, dt.TenantPath(tenant)), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	dt.Git(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-am", "the tenant adopts the kernel realm")
	dt.Git(t, seed, "push", "origin", "HEAD:main")
}

// The platform tenant takes no catalogue apps. Somebody who may install apps
// in it is told so, in a sentence that says why and what to do instead, and
// nothing is fetched from a catalogue or committed for the install.
func TestAnInstallInThePlatformTenantIsRefusedWithTheReason(t *testing.T) {
	src := addonSource(t, map[string]string{"element": elementProfile})
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	adoptKernelRealm(t, h, "demo")
	before := h.tip(t)

	for name, body := range map[string]string{
		"a bare install":          ``,
		"an install for everyone": `{"defaultGrant": true}`,
		"a pinned install":        fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile)),
	} {
		code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, body)
		if code != http.StatusConflict {
			t.Fatalf("%s in the platform tenant: %d %v, want 409", name, code, out)
		}
		// The sentence the command line and the console show, unchanged.
		if out["error"] != gitops.ErrPlatformTenant.Error() {
			t.Fatalf("%s: the refusal reads %q", name, out["error"])
		}
	}
	msg := gitops.ErrPlatformTenant.Error()
	if !strings.Contains(msg, "platform tenant") || !strings.Contains(msg, "takes no apps") {
		t.Fatalf("the refusal does not say what was refused or why: %q", msg)
	}
	if after := h.tip(t); after != before {
		t.Fatalf("a refused install committed something: %s", dt.Git(t, "", "--git-dir", h.remote, "log", "--oneline", before+".."+after))
	}

	// Everything else about the tenant still answers: its apps are listed,
	// and the tenant beside it installs as it always did.
	if code, out := h.do(t, "GET", "/v1/tenants/demo/apps", tom, ""); code != http.StatusOK {
		t.Fatalf("listing the platform tenant's apps: %d %v", code, out)
	}
}

// Add-ons are refused on the same terms, named or pinned.
func TestAddonsInThePlatformTenantAreRefusedWithTheReason(t *testing.T) {
	talk := addonProfile("element-talk")
	src := addonSource(t, map[string]string{"element": elementProfile, "element-talk": talk})
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	adoptKernelRealm(t, h, "demo")
	before := h.tip(t)

	for name, body := range map[string]string{
		"a named addon":  `{"addons":["calendar"]}`,
		"a pinned addon": `{"addons":[` + pinnedAddon("main/element-talk", sha(talk)) + `]}`,
		"no addons":      `{"addons":[]}`,
	} {
		code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, body)
		if code != http.StatusConflict || out["error"] != gitops.ErrPlatformTenant.Error() {
			t.Fatalf("%s in the platform tenant: %d %v, want 409 with the reason", name, code, out)
		}
	}
	if after := h.tip(t); after != before {
		t.Fatal("a refused addon selection committed something")
	}
}

// What makes a tenant the platform tenant is the realm its manifest adopts,
// not what it is called: a user tenant installs as before, and one that names
// its own realm -- which is what the director writes for a new tenant -- does
// too. And the caller is still asked about first: a stranger learns nothing
// about which tenant is the platform's.
func TestOnlyATenantThatAdoptsAnotherRealmIsRefused(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", tom, ""); code != http.StatusAccepted {
		t.Fatalf("an install in a user tenant: %d %v", code, out)
	}
	adoptKernelRealm(t, h, "demo")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/wiki", h.token(t, "tenant-demo", "olaf"), ""); code != http.StatusForbidden {
		t.Fatalf("a stranger asking the platform tenant: %d %v, want 403", code, out)
	}
	// Removing what is there is not an install, and stays possible.
	if code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/nextcloud", tom, ""); code != http.StatusAccepted {
		t.Fatalf("an uninstall in the platform tenant: %d %v", code, out)
	}
}
