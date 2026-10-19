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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// rolloutsOperator answers the two reads the update report makes of the
// operator, with what a test sets.
type rolloutsOperator struct {
	importingOperator
	mu             sync.Mutex
	rollouts       []map[string]any
	customizations []map[string]any
}

func (o *rolloutsOperator) set(rollouts []map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rollouts = rollouts
}

func (o *rolloutsOperator) Get(ctx context.Context, path string, q url.Values) (int, []byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch path {
	case "/v1/rollouts":
		raw, _ := json.Marshal(map[string]any{"rollouts": orEmpty(o.rollouts)})
		return http.StatusOK, raw, nil
	case "/v1/customizations":
		raw, _ := json.Marshal(map[string]any{"records": orEmpty(o.customizations)})
		return http.StatusOK, raw, nil
	}
	return o.importingOperator.Get(ctx, path, q)
}

func orEmpty(list []map[string]any) []map[string]any {
	if list == nil {
		return []map[string]any{}
	}
	return list
}

// appNamed is one entry of a tenant's manifest, read fresh.
func appNamed(t *testing.T, g *gitops.GitOps, tenant, profile string) gitops.App {
	t.Helper()
	apps, err := g.Apps(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		if a.Profile == profile {
			return a
		}
	}
	t.Fatalf("%s has no %s: %+v", tenant, profile, apps)
	return gitops.App{}
}

func withOperator(op api.Lifecycle) func(*api.Config) {
	return func(c *api.Config) { c.Lifecycle = op; c.UpdaterOff = true }
}

func updateEntryOf(t *testing.T, out map[string]any, tenant, profile string) map[string]any {
	t.Helper()
	for _, raw := range out["entries"].([]any) {
		e := raw.(map[string]any)
		if e["tenant"] == tenant && e["profile"] == profile {
			return e
		}
	}
	t.Fatalf("no entry for %s/%s in %v", tenant, profile, out["entries"])
	return nil
}

// An app pinned to a build its catalogue has moved past is reported as
// outdated, under the mode the cluster gives it. Under latest, the default,
// a tenant's administrator cannot move it; the cluster's can, and the
// report then says it is current.
func TestOutdatedAppsAreReportedAndMovedUnderTheirMode(t *testing.T) {
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
	op.set([]map[string]any{{"tenant": "demo", "profile": "notes", "pinned": sha(one), "running": sha(one), "phase": "Done", "ready": true}})
	content["gentian"]["notes"] = two

	code, out := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/updates", alice, "")
	if code != http.StatusOK {
		t.Fatalf("report = %d %v", code, out)
	}
	e := updateEntryOf(t, out, "demo", "notes")
	if e["outdated"] != true || e["newest"] != sha(two) || e["pinned"] != sha(one) || e["running"] != sha(one) ||
		e["mode"] != "latest" || e["modeFrom"] != "cluster" || e["held"] != nil {
		t.Fatalf("entry = %v", e)
	}
	if out["windowOpen"] != true {
		t.Fatalf("no window set and the report says closed: %v", out["windowOpen"])
	}
	// The tenant's report says the same, to the tenant.
	if code, out := h.do(t, "GET", "/v1/tenants/demo/updates", tom, ""); code != http.StatusOK || updateEntryOf(t, out, "demo", "notes")["outdated"] != true {
		t.Fatalf("tenant report = %d %v", code, out)
	}

	// Under latest the move is not the tenant administrator's.
	if code, out := install(tom, two); code != http.StatusForbidden || !strings.Contains(fmt.Sprint(out["error"]), "latest mode") {
		t.Fatalf("a tenant administrator moved an app in latest mode: %d %v", code, out)
	}
	// The cluster's administrator moves it, and the report follows.
	if code, out := install(alice, two); code != http.StatusAccepted || out["status"] != "updated" {
		t.Fatalf("the cluster's administrator: %d %v", code, out)
	}
	_, out = h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/updates", alice, "")
	if e := updateEntryOf(t, out, "demo", "notes"); e["outdated"] != false || e["pinned"] != sha(two) {
		t.Fatalf("after the move: %v", e)
	}
	notes := appNamed(t, gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{}), "demo", "notes")
	if !strings.HasPrefix(notes.MovedBy, "alice") || notes.MovedAt == "" {
		t.Fatalf("the manifest's record of the move: %+v", notes)
	}
}

// The tenant mode, written by the cluster's administrator for one tenant's
// app, lets the tenant's administrator move it; a tenant's administrator
// cannot write it for themselves.
func TestTheTenantModeIsTheClusterAdministratorsToGive(t *testing.T) {
	one := profileNamed("notes", "one")
	two := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "2.0.0"`)
	content := served{"gentian": {"notes": one}}
	src := catalogues(t, content)
	h := startCatalogues(t, src, withOperator(&rolloutsOperator{}))
	tom, alice := h.token(t, "tenant-demo", "tom"), h.token(t, "gentian", "alice")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes", tom, fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(one))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	content["gentian"]["notes"] = two
	modeRoute := "/v1/clusters/" + dt.Cluster + "/tenants/demo/apps/notes/update-mode"
	if code, _ := h.do(t, "PUT", modeRoute, tom, `{"mode":"tenant"}`); code != http.StatusForbidden {
		t.Fatalf("a tenant administrator set the mode: %d", code)
	}
	if code, out := h.do(t, "PUT", modeRoute, alice, `{"mode":"bogus"}`); code != http.StatusBadRequest {
		t.Fatalf("a mode that is none: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", modeRoute, alice, `{"mode":"tenant"}`); code != http.StatusAccepted {
		t.Fatalf("set the mode: %d %v", code, out)
	}
	_, out := h.do(t, "GET", "/v1/tenants/demo/updates", tom, "")
	if e := updateEntryOf(t, out, "demo", "notes"); e["mode"] != "tenant" || e["modeFrom"] != "tenant" {
		t.Fatalf("entry after the override: %v", e)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes", tom, fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(two))); code != http.StatusAccepted {
		t.Fatalf("the tenant's administrator under tenant mode: %d %v", code, out)
	}
	// Removed, the cluster's policy applies again.
	if code, out := h.do(t, "PUT", modeRoute, alice, `{"mode":""}`); code != http.StatusAccepted {
		t.Fatalf("remove the override: %d %v", code, out)
	}
	_, out = h.do(t, "GET", "/v1/tenants/demo/updates", tom, "")
	if e := updateEntryOf(t, out, "demo", "notes"); e["modeFrom"] != "cluster" {
		t.Fatalf("entry after the override went: %v", e)
	}
}

// A customization at L3 or above that carries upgrade risk puts its app on
// a manual move, and the report says which record.
func TestACustomizationWithUpgradeRiskHoldsItsAppOnAManualMove(t *testing.T) {
	one := profileNamed("notes", "one")
	two := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "2.0.0"`)
	content := served{"gentian": {"notes": one}}
	src := catalogues(t, content)
	op := &rolloutsOperator{customizations: []map[string]any{
		{"name": "notes.ledger-module", "targetProfile": "notes", "rung": "L3", "upgradeRisk": "high", "tenants": []string{"demo"}},
	}}
	h := startCatalogues(t, src, withOperator(op))
	tom, alice := h.token(t, "tenant-demo", "tom"), h.token(t, "gentian", "alice")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes", tom, fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(one))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	content["gentian"]["notes"] = two
	_, out := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/updates", alice, "")
	e := updateEntryOf(t, out, "demo", "notes")
	held, _ := e["held"].([]any)
	if e["mode"] != "manual" || e["modeFrom"] != "customization" || len(held) != 1 || !strings.Contains(fmt.Sprint(held[0]), "notes.ledger-module") {
		t.Fatalf("entry = %v", e)
	}
	// The platform's mover leaves it alone; the administrator's move takes it.
	if moved := h.server().RunLatestMoves(context.Background()); moved != 0 {
		t.Fatalf("the mover moved a held app: %d", moved)
	}
	code, out := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/actions/move-outdated", alice, `{"apps":["notes"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("move-outdated = %d %v", code, out)
	}
	moved, _ := out["moved"].([]any)
	if len(moved) != 1 || moved[0].(map[string]any)["to"] != sha(two) {
		t.Fatalf("moved = %v", out)
	}
}

// The platform's own mover: one tenant first; the rest once that one runs
// the build; nobody while a move to the build was rolled back.
func TestThePlatformMovesOneTenantFirstAndStopsOnARollback(t *testing.T) {
	one := profileNamed("notes", "one")
	two := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "2.0.0"`)
	content := served{"gentian": {"notes": one}}
	src := catalogues(t, content)
	op := &rolloutsOperator{}
	h := startCatalogues(t, src, withOperator(op))
	tom, tina := h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	for tenant, token := range map[string]string{"demo": tom, "solo": tina} {
		if code, out := h.do(t, "POST", "/v1/tenants/"+tenant+"/apps/notes", token, fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(one))); code != http.StatusAccepted {
			t.Fatalf("%s: install = %d %v", tenant, code, out)
		}
	}
	running := func(tenant, pinned, run, phase string) map[string]any {
		return map[string]any{"tenant": tenant, "profile": "notes", "pinned": pinned, "running": run, "target": pinned, "phase": phase, "ready": true}
	}
	op.set([]map[string]any{running("demo", sha(one), sha(one), "Done"), running("solo", sha(one), sha(one), "Done")})
	content["gentian"]["notes"] = two
	srv := h.server()

	// The canary: one tenant, the first by name.
	if moved := srv.RunLatestMoves(context.Background()); moved != 1 {
		t.Fatalf("first run moved %d, want the one canary", moved)
	}
	g := gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{})
	pinned := func(tenant string) string { return appNamed(t, g, tenant, "notes").Digest }
	if pinned("demo") != sha(two) || pinned("solo") != sha(one) {
		t.Fatalf("after the canary: demo %s, solo %s", pinned("demo"), pinned("solo"))
	}
	if by := appNamed(t, g, "demo", "notes").MovedBy; by != "policy:latest" {
		t.Fatalf("the mover's commit names %q", by)
	}
	// While the canary rolls, nobody else moves.
	op.set([]map[string]any{running("demo", sha(two), sha(one), "Rolling"), running("solo", sha(one), sha(one), "Done")})
	if moved := srv.RunLatestMoves(context.Background()); moved != 0 {
		t.Fatalf("moved %d while the canary rolls", moved)
	}
	// The canary was rolled back: the rest stay where they are.
	op.set([]map[string]any{running("demo", sha(two), sha(one), "RolledBack"), running("solo", sha(one), sha(one), "Done")})
	if moved := srv.RunLatestMoves(context.Background()); moved != 0 {
		t.Fatalf("moved %d after a rollback", moved)
	}
	// The canary runs the build: the rest follow.
	op.set([]map[string]any{running("demo", sha(two), sha(two), "Done"), running("solo", sha(one), sha(one), "Done")})
	if moved := srv.RunLatestMoves(context.Background()); moved != 1 || pinned("solo") != sha(two) {
		t.Fatalf("after the canary came up: moved %d, solo at %s", moved, pinned("solo"))
	}
}
