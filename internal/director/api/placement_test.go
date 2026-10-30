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
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/placement"
)

// placingOperator answers what the platform places as a test sets it.
type placingOperator struct {
	importingOperator
	pmu    sync.Mutex
	answer placement.Answer
}

func (o *placingOperator) set(a placement.Answer) {
	o.pmu.Lock()
	defer o.pmu.Unlock()
	o.answer = a
}

func (o *placingOperator) Get(ctx context.Context, path string, q url.Values) (int, []byte, error) {
	if path == "/v1/placements" {
		o.pmu.Lock()
		defer o.pmu.Unlock()
		raw, _ := json.Marshal(o.answer)
		return http.StatusOK, raw, nil
	}
	return o.importingOperator.Get(ctx, path, q)
}

func set(wanted ...string) placement.Set {
	if wanted == nil {
		wanted = []string{}
	}
	return placement.Set{Wanted: wanted, Undecided: []string{}}
}

// platformTenantYAML is the platform tenant: the one whose realm is the
// kernel's.
const platformTenantYAML = `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: platform
spec:
  displayName: Platform
  isolation:
    keycloakRealm: kernel
  apps: []
`

func startPlacing(t *testing.T, op *placingOperator) *harness {
	t.Helper()
	return startSeeded(t, op, func(remote string) {
		dt.Commit(t, remote, map[string]string{dt.TenantPath("platform"): platformTenantYAML})
	}, func(c *api.Config) { c.UpdaterOff = true })
}

func placedApp(t *testing.T, g *gitops.GitOps, tenant, profile string) (gitops.App, bool) {
	t.Helper()
	apps, err := g.Apps(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		if a.Profile == profile {
			return a, true
		}
	}
	return gitops.App{}, false
}

// The platform writes what it places into every tenant's manifest: the
// user tenants' components into the user tenants, the platform tenant's
// into it, each entry marked placed and pinned to nothing. Placed once, a
// second pass changes nothing; withdrawn, it is taken away -- but not while
// its placement cannot be decided.
func TestThePlatformWritesWhatItPlacesIntoEveryTenant(t *testing.T) {
	op := &placingOperator{}
	op.set(placement.Answer{Users: set("desktop", "store"), Platform: set("concierge", "desktop")})
	h := startPlacing(t, op)
	srv := h.server()

	users := []string{"demo", "solo", "other"}
	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != 2*len(users)+2 || removed != 0 {
		t.Fatalf("placed %d and removed %d, want %d and 0", placed, removed, 2*len(users)+2)
	}
	g := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	for _, tenant := range users {
		for _, name := range []string{"desktop", "store"} {
			a, ok := placedApp(t, g, tenant, name)
			if !ok || !a.Placed || a.Digest != "" || a.DefaultGrant {
				t.Errorf("%s/%s = %+v (present %v), want placed and pinned to nothing", tenant, name, a, ok)
			}
		}
		if _, ok := placedApp(t, g, tenant, "concierge"); ok {
			t.Errorf("%s has the platform tenant's concierge", tenant)
		}
	}
	for _, name := range []string{"concierge", "desktop"} {
		if a, ok := placedApp(t, g, "platform", name); !ok || !a.Placed {
			t.Errorf("platform/%s = %+v (present %v), want placed", name, a, ok)
		}
	}
	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != 0 || removed != 0 {
		t.Fatalf("a second pass placed %d and removed %d", placed, removed)
	}

	// The store cannot be decided: what was placed of it stays.
	op.set(placement.Answer{Users: placement.Set{Wanted: []string{"desktop"}, Undecided: []string{"store"}}, Platform: set("concierge", "desktop")})
	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != 0 || removed != 0 {
		t.Fatalf("an undecided pass placed %d and removed %d", placed, removed)
	}
	// Decided against: it goes on the second pass that finds it so, and
	// only it -- the tenant's own app stays.
	op.set(placement.Answer{Users: set("desktop"), Platform: set("concierge", "desktop")})
	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != 0 || removed != 0 {
		t.Fatalf("the first pass to find it withdrawn placed %d and removed %d, want nothing", placed, removed)
	}
	if placed, removed := srv.PlacePlatformApps(context.Background()); placed != 0 || removed != len(users) {
		t.Fatalf("placed %d and removed %d, want 0 and %d", placed, removed, len(users))
	}
	g = gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	for _, tenant := range users {
		if _, ok := placedApp(t, g, tenant, "store"); ok {
			t.Errorf("%s still has the store", tenant)
		}
		if _, ok := placedApp(t, g, tenant, "nextcloud"); !ok {
			t.Errorf("%s lost its own nextcloud", tenant)
		}
	}
}

// Nobody changes a placed entry by hand: not its tenant's administrator and
// not the cluster's. Uninstalling, moving it, its add-ons and its update
// mode are refused, and the app lists show the platform's own components as
// they always did: not among the tenant's apps.
func TestNobodyChangesAPlacedEntryByHand(t *testing.T) {
	op := &placingOperator{}
	op.set(placement.Answer{Users: set("desktop"), Platform: set("desktop")})
	h := startPlacing(t, op)
	h.server().PlacePlatformApps(context.Background())

	tom := h.token(t, "tenant-demo", "tom")
	alice := h.token(t, "gentian", "alice")
	for _, who := range []string{tom, alice} {
		if code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/desktop", who, ""); code != http.StatusConflict ||
			!strings.Contains(out["error"].(string), "comes with the platform") {
			t.Errorf("uninstall = %d %v, want 409", code, out)
		}
		if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/desktop/addons", who, `{"addons": []}`); code != http.StatusConflict {
			t.Errorf("addons = %d %v, want 409", code, out)
		}
	}
	code, out := h.do(t, "GET", "/v1/tenants/demo/apps", tom, "")
	if code != http.StatusOK {
		t.Fatalf("apps = %d %v", code, out)
	}
	for _, raw := range out["apps"].([]any) {
		if raw.(map[string]any)["profile"] == "desktop" {
			t.Errorf("the desktop is listed among the tenant's apps: %v", out["apps"])
		}
	}

	g := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	meta := gitops.Meta{Principal: "someone"}
	ctx := context.Background()
	for what, err := range map[string]error{
		"install":     func() error { _, err := g.Install(ctx, "demo", "desktop", "", meta); return err }(),
		"update mode": func() error { _, err := g.SetAppUpdateMode(ctx, "demo", "desktop", "manual", meta); return err }(),
	} {
		if !errors.Is(err, gitops.ErrPlacedApp) {
			t.Errorf("%s of a placed entry: %v, want ErrPlacedApp", what, err)
		}
	}
	details, err := g.TenantDetails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range details {
		if d.Name == "demo" && (strings.Contains(strings.Join(d.Apps, ","), "desktop") || len(d.Placed) != 1) {
			t.Errorf("demo lists apps %v and placed %v, want the desktop placed only", d.Apps, d.Placed)
		}
	}
}

// A tenant created through the director has what the platform places from
// its first commit.
func TestANewTenantHasItsPlacedComponentsFromTheCommitThatMakesIt(t *testing.T) {
	op := &placingOperator{}
	op.set(placement.Answer{Users: set("admin-console", "desktop"), Platform: set("desktop")})
	h := startPlacing(t, op)
	alice := h.token(t, "gentian", "alice")
	code, out := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants", alice, `{"name": "fresh", "displayName": "Fresh"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d %v", code, out)
	}
	commit := out["commit"].(string)
	manifest := dt.Git(t, "", "--git-dir", h.remote, "show", commit+":"+dt.TenantPath("fresh"))
	for _, name := range []string{"admin-console", "desktop"} {
		if !strings.Contains(manifest, "- profile: "+name+"\n    placed: true") {
			t.Errorf("the creating commit does not place %s:\n%s", name, manifest)
		}
	}
}

// An entry of the name the tenant installed itself is the tenant's: the
// platform does not mark it placed, and does not take it away when its
// placement is withdrawn.
func TestThePlatformDoesNotTakeOverATenantsOwnInstall(t *testing.T) {
	op := &placingOperator{}
	op.set(placement.Answer{Users: set("nextcloud"), Platform: set()})
	h := startPlacing(t, op)
	srv := h.server()
	srv.PlacePlatformApps(context.Background())
	op.set(placement.Answer{Users: set(), Platform: set()})
	srv.PlacePlatformApps(context.Background())
	srv.PlacePlatformApps(context.Background())
	g := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	for _, tenant := range []string{"demo", "solo", "other"} {
		if a, ok := placedApp(t, g, tenant, "nextcloud"); !ok || a.Placed {
			t.Errorf("%s/nextcloud = %+v (present %v), want the tenant's own, unmarked", tenant, a, ok)
		}
	}
}
