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
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// The removal a tenant's administrator may make of what a newer build of an
// app left behind. Tenant user carries the name of the one user tenant of a
// single-tenancy cluster: uma administers it and ulf is a member.

const removeShopResidue = "/v1/tenants/user/apps/shop/actions/remove-residue"

const oldPage = `{"kind":"ConfigMap","name":"shop.old-page","confirm":"shop.old-page"}`

// startTenancy is the harness on a cluster whose claim declares the tenancy
// mode given ("" declares none), with tenant user in the repository.
func startTenancy(t *testing.T, op *residueOperator, mode string, more map[string]string) *harness {
	t.Helper()
	return startSeeded(t, op, func(remote string) {
		claim := claimWith()
		if mode != "" {
			claim += "  tenancyMode: " + mode + "\n"
		}
		files := map[string]string{dt.ClaimPath: claim, dt.TenantPath("user"): dt.TenantYAML("user")}
		for path, body := range more {
			files[path] = body
		}
		dt.Commit(t, remote, files)
	})
}

// The route asks can_install_app of the tenant, as uninstalling and purging
// an app do, and nobody without it reaches anything.
func TestATenantsRemovalAsksWhoeverMayInstall(t *testing.T) {
	op := &residueOperator{answers: []residueAnswer{deletedAnswer("ConfigMap", "shop.old-page")}}
	h := startTenancy(t, op, "single", nil)
	before := h.tip(t)

	for who, token := range map[string]string{
		"a member of the tenant":           h.token(t, "tenant-user", "ulf"),
		"another tenant's administrator":   h.token(t, "tenant-demo", "tom"),
		"an auditor of the cluster":        h.token(t, "gentian", "audrey"),
		"somebody with no relation at all": h.token(t, "tenant-user", "nobody"),
	} {
		if code, _ := h.do(t, "POST", removeShopResidue, token, oldPage); code != http.StatusForbidden {
			t.Errorf("%s removed a leftover: %d", who, code)
		}
	}
	if code, _ := h.do(t, "POST", removeShopResidue, "", oldPage); code != http.StatusUnauthorized {
		t.Errorf("nobody removed a leftover: %d", code)
	}
	if len(op.removals()) != 0 {
		t.Fatalf("a refused caller reached the operator: %v", op.removals())
	}

	h.asked.reset()
	code, out := h.do(t, "POST", removeShopResidue, h.token(t, "tenant-user", "uma"), oldPage)
	if code != http.StatusAccepted || out["status"] != "deleted" {
		t.Fatalf("the tenant's administrator: %d %v", code, out)
	}
	if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:uma can_install_app tenant:user" {
		t.Errorf("asked %v", asked)
	}
	if h.tip(t) != before {
		t.Fatal("removing a leftover wrote to git")
	}
}

// Where the cluster carries more than one user tenant the pieces are every
// tenant's, and the relation on one of them removes nothing: 403, with the
// sentence, before the name is asked for and without asking the operator.
func TestATenantsRemovalIsRefusedUnlessTheTenantIsTheClustersOnlyOne(t *testing.T) {
	for _, c := range []struct {
		mode, path, realm, who string
	}{
		{"multi", removeShopResidue, "tenant-user", "uma"},
		{"", removeShopResidue, "tenant-user", "uma"},
		// The platform's administrator, who holds the relation through
		// operated_by: the cluster's own route is theirs, not this one.
		{"multi", removeShopResidue, "gentian", "alice"},
		// Single, and not the tenant such a cluster carries.
		{"single", "/v1/tenants/demo/apps/shop/actions/remove-residue", "tenant-demo", "tom"},
	} {
		op := &residueOperator{answers: []residueAnswer{deletedAnswer("ConfigMap", "shop.old-page")}}
		h := startTenancy(t, op, c.mode, nil)
		token := h.token(t, c.realm, c.who)
		for _, body := range []string{oldPage, `{"kind":"ConfigMap","name":"shop.old-page"}`, `{}`} {
			code, out := h.do(t, "POST", c.path, token, body)
			if code != http.StatusForbidden || out["error"] != tenancy.SharedPieces || out["reason"] != "shared" || out["removableBy"] != "platform" {
				t.Errorf("mode %q, %s, body %s: %d %v", c.mode, c.who, body, code, out)
			}
		}
		if len(op.removals()) != 0 {
			t.Errorf("mode %q, %s: the operator was asked: %v", c.mode, c.who, op.removals())
		}
	}
}

// The name is typed again before anything is asked of the cluster, and only
// a companion can be named: a profile is the cluster's to retire.
func TestATenantsRemovalAsksForTheNameAgain(t *testing.T) {
	op := &residueOperator{answers: []residueAnswer{deletedAnswer("ConfigMap", "shop.old-page")}}
	h := startTenancy(t, op, "single", nil)
	uma := h.token(t, "tenant-user", "uma")

	for _, body := range []string{
		`{"kind":"ConfigMap","name":"shop.old-page"}`,
		`{"kind":"ConfigMap","name":"shop.old-page","confirm":"shop"}`,
		`{"kind":"ConfigMap","name":"shop.old-page","confirm":"yes"}`,
	} {
		code, out := h.do(t, "POST", removeShopResidue, uma, body)
		if code != http.StatusPreconditionRequired || out["confirmField"] != "confirm" || out["confirmWith"] != "shop.old-page" ||
			out["dangerous"] != true || out["requiresRetype"] != true {
			t.Fatalf("unconfirmed %s: %d %v", body, code, out)
		}
		// What it costs is said: a build that needed the piece.
		if said := fmt.Sprint(out["error"]); !strings.Contains(said, "deletes it from the cluster") || !strings.Contains(said, "older") {
			t.Errorf("the warning: %s", said)
		}
	}
	for _, body := range []string{
		``, `{}`,
		`{"kind":"ComponentProfile","name":"shop","confirm":"shop"}`,
		`{"kind":"Secret","name":"keycloak-admin","confirm":"keycloak-admin"}`,
		`{"kind":"Composition","name":"../x","confirm":"../x"}`,
		`{"kind":"ConfigMap","name":"a.b","namespace":"Not A Namespace","confirm":"a.b"}`,
		// The scope is the route's, never the body's.
		`{"kind":"ConfigMap","name":"shop.old-page","confirm":"shop.old-page","tenant":"demo","profile":"notes"}`,
		`{"kind":"ConfigMap","name":"shop.old-page","confirm":"shop.old-page","profile":"notes"}`,
	} {
		if code, out := h.do(t, "POST", removeShopResidue, uma, body); code != http.StatusBadRequest {
			t.Errorf("body %q: %d %v", body, code, out)
		}
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/user/apps/Not%20A%20Name/actions/remove-residue", uma, oldPage); code != http.StatusBadRequest {
		t.Errorf("a profile that is not a name: %d", code)
	}
	if len(op.removals()) != 0 {
		t.Fatalf("an unconfirmed or malformed request reached the operator: %v", op.removals())
	}
}

// It is the cluster's removal of a companion with the tenant and the app
// named in it: the operator is asked once, as the person, and what it says
// -- done, or refused and why -- is the answer.
func TestATenantsRemovalIsTheOperatorsWithTheTenantAndTheAppNamed(t *testing.T) {
	op := &residueOperator{answers: []residueAnswer{
		deletedAnswer("ConfigMap", "shop.old-page"),
		{http.StatusOK, `{"status":"deleting","deleted":{"kind":"Customization","name":"shop.old"},"message":"the API server took the deletion"}`},
		refusedAnswer("not-residue", "the Composition app-odoo is not something a newer build of shop left behind. Nothing was deleted"),
		refusedAnswer("still-declared", "Argo CD finds the ConfigMap shop.old-page declared. Nothing was deleted"),
		refusedAnswer("changed", "the ConfigMap shop.old-page was changed by something else. Nothing was deleted by this request"),
		{http.StatusNotFound, `{"detail":"not installed in this tenant: shop in user"}`},
		{http.StatusForbidden, fmt.Sprintf(`{"detail":%q,"reason":"shared"}`, tenancy.SharedPieces)},
	}}
	h := startTenancy(t, op, "single", nil)
	uma := h.token(t, "tenant-user", "uma")
	before := h.tip(t)

	code, out := h.do(t, "POST", removeShopResidue, uma,
		`{"kind":"ConfigMap","name":"shop.old-page","namespace":"kernel-provisioning","confirm":"shop.old-page"}`)
	deleted, _ := out["deleted"].(map[string]any)
	if code != http.StatusAccepted || out["status"] != "deleted" || deleted["name"] != "shop.old-page" || out["message"] == "" {
		t.Fatalf("removal: %d %v", code, out)
	}
	want := `uma@example.com: {"kind":"ConfigMap","name":"shop.old-page","namespace":"kernel-provisioning","tenant":"user","profile":"shop"}`
	if got := op.removals(); len(got) != 1 || got[0] != want {
		t.Fatalf("the operator was asked %v, want %s", got, want)
	}
	// Taken by the API server and not gone yet is said as that.
	code, out = h.do(t, "POST", removeShopResidue, uma, `{"kind":"Customization","name":"shop.old","confirm":"shop.old"}`)
	if code != http.StatusAccepted || out["status"] != "deleting" {
		t.Fatalf("held by a finalizer: %d %v", code, out)
	}

	for _, c := range []struct {
		body, reason, says string
	}{
		{`{"kind":"Composition","name":"app-odoo","confirm":"app-odoo"}`, "not-residue", "not something a newer build of shop"},
		{oldPage, "still-declared", "Argo CD"},
		{oldPage, "changed", "changed by something else"},
	} {
		code, out := h.do(t, "POST", removeShopResidue, uma, c.body)
		if code != http.StatusConflict || out["reason"] != c.reason || !strings.Contains(fmt.Sprint(out["error"]), c.says) {
			t.Fatalf("refused as %s: %d %v", c.reason, code, out)
		}
	}
	// An app the tenant does not have.
	if code, out := h.do(t, "POST", removeShopResidue, uma, oldPage); code != http.StatusNotFound || !strings.Contains(fmt.Sprint(out["error"]), "not installed") {
		t.Fatalf("not installed: %d %v", code, out)
	}
	// The operator runs under another mode than git declares: its word.
	if code, out := h.do(t, "POST", removeShopResidue, uma, oldPage); code != http.StatusForbidden || out["error"] != tenancy.SharedPieces || out["removableBy"] != "platform" {
		t.Fatalf("refused by the operator's own mode: %d %v", code, out)
	}
	if len(op.removals()) != 7 {
		t.Fatalf("the operator was asked %d times", len(op.removals()))
	}
	if h.tip(t) != before {
		t.Fatal("removing a leftover wrote to git")
	}
}

// What a bundle file in the deployments repository declares is owned, for a
// tenant's removal as for the cluster's: the operator is not asked.
func TestATenantsRemovalLeavesWhatGitStillDeclares(t *testing.T) {
	op := &residueOperator{answers: []residueAnswer{deletedAnswer("ConfigMap", "shop.theme")}}
	h := startTenancy(t, op, "single", map[string]string{dt.CataloguePath("shop.yaml"): shopThemeBundle})
	uma := h.token(t, "tenant-user", "uma")

	code, out := h.do(t, "POST", removeShopResidue, uma, `{"kind":"ConfigMap","name":"shop.theme","confirm":"shop.theme"}`)
	if code != http.StatusConflict || out["reason"] != "still-declared" || !strings.Contains(fmt.Sprint(out["error"]), "bundle of profile shop") {
		t.Fatalf("a declared companion: %d %v", code, out)
	}
	if len(op.removals()) != 0 {
		t.Fatalf("the operator was asked to delete what git declares: %v", op.removals())
	}
}
