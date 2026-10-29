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

// An app pinned to a build its catalog has moved past is reported as
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

// An app the catalog lists for the store alone -- an ee entry, acquired
// and installed through the App Store -- moves like any other once it is
// installed: the report names its newest build and move-outdated moves it.
// The acquisition is per app, not per build. For a tenant that does not have
// it, the entry stays out of the listing, so a new install of it is still
// the store's.
func TestAnInstalledStoreOnlyAppMovesLikeAnyOther(t *testing.T) {
	one := "# edition: ee\n" + profileNamed("ledger", "one")
	two := strings.ReplaceAll(one, `version: "1.0.0"`, `version: "2.0.0"`)
	content := served{"gentian": {"ledger": one}}
	src := catalogues(t, content)
	op := &rolloutsOperator{}
	h := startCatalogues(t, src, withOperator(op))
	alice := h.token(t, "gentian", "alice")
	// The store installs by coordinate and digest, through the same route.
	// Nothing reads the index until the catalog has moved on: it is cached
	// once read.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/ledger", alice,
		fmt.Sprintf(`{"coordinate":"gentian/ledger","digest":%q}`, sha(one))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	op.set([]map[string]any{{"tenant": "demo", "profile": "ledger", "pinned": sha(one), "running": sha(one), "phase": "Done", "ready": true}})
	content["gentian"]["ledger"] = two
	listed := func() (int, int) {
		_, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/gentian/entries", alice, "")
		return len(out["entries"].([]any)), int(out["storeOnly"].(float64))
	}
	if entries, storeOnly := listed(); entries != 0 || storeOnly != 1 {
		t.Fatalf("the listing has %d entries and %d store-only; want 0 and 1", entries, storeOnly)
	}
	code, out := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/updates", alice, "")
	if code != http.StatusOK {
		t.Fatalf("report = %d %v", code, out)
	}
	if e := updateEntryOf(t, out, "demo", "ledger"); e["outdated"] != true || e["newest"] != sha(two) || e["catalogue"] != "gentian" || e["held"] != nil {
		t.Fatalf("an installed store-only app is not reported outdated: %v", e)
	}
	code, out = h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/actions/move-outdated", alice, `{"apps":["ledger"]}`)
	if code != http.StatusAccepted || len(out["moved"].([]any)) != 1 || len(out["skipped"].([]any)) != 0 {
		t.Fatalf("move-outdated = %d %v", code, out)
	}
	if moved := out["moved"].([]any)[0].(map[string]any); moved["to"] != sha(two) || moved["status"] != "updated" {
		t.Fatalf("moved = %v", moved)
	}
	_, out = h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/updates", alice, "")
	if e := updateEntryOf(t, out, "demo", "ledger"); e["outdated"] != false || e["pinned"] != sha(two) {
		t.Fatalf("after the move: %v", e)
	}
	// Still not listed: the move changed nothing about who installs it anew.
	if entries, storeOnly := listed(); entries != 0 || storeOnly != 1 {
		t.Fatalf("after the move the listing has %d entries and %d store-only; want 0 and 1", entries, storeOnly)
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

// A pinned add-on is reported beside its app and moves under its app's mode:
// the platform moves it in latest mode and not in manual, and move-outdated
// moves it in any, each moving that add-on's pin and leaving the app and its
// other add-ons where they are.
func TestAPinnedAddonIsReportedAndMovedUnderItsAppsMode(t *testing.T) {
	notes := profileNamed("notes", "one")
	addon := func(name, version string) string {
		return strings.ReplaceAll(strings.ReplaceAll(addonProfile(name), "of: element", "of: notes"),
			`version: "1.0.0"`, `version: "`+version+`"`)
	}
	talk, deck := addon("notes-talk", "1.0.0"), addon("notes-deck", "1.0.0")
	content := served{"gentian": {"notes": notes, "notes-talk": talk, "notes-deck": deck}}
	src := catalogues(t, content)
	op := &rolloutsOperator{}
	h := startCatalogues(t, src, withOperator(op))
	alice := h.token(t, "gentian", "alice")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes", alice,
		fmt.Sprintf(`{"coordinate":"gentian/notes","digest":%q}`, sha(notes))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/notes/addons", alice,
		`{"addons":[`+pinnedAddon("gentian/notes-talk", sha(talk))+`,`+pinnedAddon("gentian/notes-deck", sha(deck))+`]}`); code != http.StatusAccepted {
		t.Fatalf("set addons = %d %v", code, out)
	}
	op.set([]map[string]any{{"tenant": "demo", "profile": "notes", "pinned": sha(notes), "running": sha(notes), "phase": "Done", "ready": true}})
	// Nothing reads the index until the catalog has moved on: it is
	// cached once read.
	talk2, deck2 := addon("notes-talk", "2.0.0"), addon("notes-deck", "2.0.0")
	content["gentian"]["notes-talk"], content["gentian"]["notes-deck"] = talk2, deck2
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/apps/notes/update-mode"), alice, `{"mode":"manual"}`); code >= 300 {
		t.Fatalf("set mode = %d %v", code, out)
	}

	_, out := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/updates", alice, "")
	if e := updateEntryOf(t, out, "demo", "notes-talk"); e["addonOf"] != "notes" || e["outdated"] != true ||
		e["pinned"] != sha(talk) || e["newest"] != sha(talk2) || e["mode"] != "manual" || e["modeFrom"] != "tenant" {
		t.Fatalf("add-on entry = %v", e)
	}
	if e := updateEntryOf(t, out, "demo", "notes"); e["outdated"] != false || e["addonOf"] != nil {
		t.Fatalf("app entry = %v", e)
	}
	order := []string{}
	for _, raw := range out["entries"].([]any) {
		order = append(order, raw.(map[string]any)["profile"].(string))
	}
	if got := strings.Join(order, " "); got != "notes notes-deck notes-talk" {
		t.Fatalf("entries in order %q, want each app's add-ons after it", got)
	}

	pins := func() map[string]string {
		out := map[string]string{}
		for _, p := range appNamed(t, gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{}), "demo", "notes").AddonPins {
			out[p.Name] = p.Digest
		}
		return out
	}
	// In manual mode the platform leaves them; the administrator moves one.
	if moved := h.server().RunLatestMoves(context.Background()); moved != 0 {
		t.Fatalf("an add-on of a manual-mode app was moved by the platform: %d", moved)
	}
	code, out := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/actions/move-outdated", alice, `{"apps":["notes-deck"]}`)
	if code != http.StatusAccepted || len(out["moved"].([]any)) != 1 || len(out["skipped"].([]any)) != 0 {
		t.Fatalf("move-outdated = %d %v", code, out)
	}
	if p := pins(); p["notes-talk"] != sha(talk) || p["notes-deck"] != sha(deck2) {
		t.Fatalf("after move-outdated: %v", p)
	}

	// Back in latest mode the platform moves the other, and only it.
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/apps/notes/update-mode"), alice, `{"mode":""}`); code >= 300 {
		t.Fatalf("clear mode = %d %v", code, out)
	}
	if moved := h.server().RunLatestMoves(context.Background()); moved != 1 {
		t.Fatalf("the mover moved %d, want the one add-on", moved)
	}
	if p := pins(); p["notes-talk"] != sha(talk2) || p["notes-deck"] != sha(deck2) {
		t.Fatalf("after the platform's move: %v", p)
	}
	if moved := h.server().RunLatestMoves(context.Background()); moved != 0 {
		t.Fatalf("a current add-on was moved again: %d", moved)
	}
	if a := appNamed(t, gitops.NewGitOps(dt.Clone(t, h.remote), h.remote, dt.Cluster, gitops.Person{}), "demo", "notes"); a.Digest != sha(notes) || len(a.Addons) != 2 {
		t.Fatalf("the app itself changed: %+v", a)
	}
}
