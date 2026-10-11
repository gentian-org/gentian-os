/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// appResidueWorld is residueWorld seen by three tenants. demo has shop with
// the add-on shop-gift, and drive. user -- the name of the one user tenant of
// a single-tenancy cluster -- has shop with the same add-on and nothing else.
// rivalco has notes, and an app that names no profile by name, which the
// cluster's list reports with the tenant's name.
//
// Beside what residueWorld leaves behind:
//
//	dropped  ConfigMap shop-gift.old (the add-on's)
//	dropped  OIDCPackCatalog shop-old-oidc (shop's; holds "shop" as shop-oidc does)
func appResidueWorld(t *testing.T, mode string, more ...client.Object) *purgeWorld {
	t.Helper()
	objects := []client.Object{
		&gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.SingleUserTenantName},
			Spec: gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{{
				Profile: "shop", Addons: []string{"shop-gift"},
				// A pin on an add-on that is not switched on installs nothing.
				AddonPins: []gentianov1alpha1.AddonPin{{Name: "drive", Digest: "sha256:" + strings.Repeat("a", 64)}},
			}}},
		},
		&gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "rivalco"},
			Spec: gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{
				{Profile: "notes"},
				{ProfileRef: &gentianov1alpha1.ProfileReference{}},
			}},
		},
		configMap(provisioningNS, "shop-gift.old", map[string]string{profilebundle.ProfileLabel: "shop-gift", profilebundle.AssetLabel: "old"}),
		packs("shop-old-oidc", of("shop"), "shop"),
	}
	w := residueWorld(t, append(objects, more...)...)
	w.svc.opts.TenancyMode = mode
	return w
}

func namesOf(items []ResidueItem) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, item.Kind+"/"+item.Name)
	}
	return out
}

// A tenant is shown what newer builds left behind of an app it has: what
// names the app or an add-on switched on inside it, of the classes dropped
// and orphaned, and nothing else of the cluster's list.
func TestATenantIsShownTheLeftoversOfAnAppItHas(t *testing.T) {
	w := appResidueWorld(t, "multi")
	ctx := context.Background()

	got, err := w.svc.AppResidue(ctx, "demo", "shop")
	if err != nil {
		t.Fatal(err)
	}
	// The app's, and its add-on's. Not app-notes (another profile's), not
	// gone-oidc (another profile's, orphaned), not what nobody owns, and no
	// unused profile.
	want := []string{"ConfigMap/shop-gift.old", "ConfigMap/shop.old-page", "OIDCPackCatalog/shop-old-oidc"}
	if names := namesOf(got.Residue); !reflect.DeepEqual(names, want) {
		t.Fatalf("shop in demo: %v, want %v", names, want)
	}
	if !reflect.DeepEqual(got.Profiles, []string{"shop", "shop-gift"}) || got.Tenant != "demo" || got.Profile != "shop" {
		t.Fatalf("about %+v", got)
	}
	for _, item := range got.Residue {
		if item.Class != ResidueDropped || item.Reason == "" || !item.Removable {
			t.Errorf("%s: %+v", item.Name, item)
		}
	}
	// A pack catalog says whether it is still read, as on the cluster's list.
	oidc := got.Residue[2].OIDC
	if oidc == nil || oidc.Effective != EffectiveContested || !reflect.DeepEqual(oidc.Contested, []string{"shop"}) || oidc.Composition != "shop" {
		t.Fatalf("the pack catalog: %+v", oidc)
	}
	if len(got.Incomplete) != 0 || got.OIDCRule == "" {
		t.Fatalf("incomplete %v, rule %q", got.Incomplete, got.OIDCRule)
	}

	// Asked for the add-on, only the add-on's.
	got, err = w.svc.AppResidue(ctx, "demo", "shop-gift")
	if err != nil || !reflect.DeepEqual(namesOf(got.Residue), []string{"ConfigMap/shop-gift.old"}) {
		t.Fatalf("the add-on: %+v, %v", got, err)
	}
	// An app with nothing left over: an empty list, not none.
	got, err = w.svc.AppResidue(ctx, "demo", "drive")
	if err != nil || got.Residue == nil || len(got.Residue) != 0 {
		t.Fatalf("drive: %+v, %v", got, err)
	}
}

// The read answers only for a profile the tenant has. Anything else is not
// found, whether or not the cluster has it: the read is no way to learn what
// is in the cluster's catalogue or what another tenant installed.
func TestTheReadAnswersOnlyForAnAppTheTenantHas(t *testing.T) {
	w := appResidueWorld(t, "multi")
	for _, c := range []struct{ tenant, profile string }{
		{"demo", "notes"},     // on the cluster, another tenant's
		{"demo", "wiki"},      // uninstalled, data retained
		{"demo", "absent"},    // not on the cluster
		{"demo", "gone"},      // names residue, installed nowhere
		{"demo", ""},          //
		{"user", "drive"},     // pinned as an add-on that is not switched on
		{"user", "notes"},     //
		{"elsewhere", "shop"}, // no such tenant
	} {
		got, err := w.svc.AppResidue(context.Background(), c.tenant, c.profile)
		if got != nil || !errors.Is(err, ErrNotInstalled) {
			t.Errorf("%s in %s: %+v, %v", c.profile, c.tenant, got, err)
		}
	}
}

// Nothing in the answer says anything about another tenant or another
// profile: not in an item, and not in what could not be established, which
// on the cluster's list names both.
func TestTheAnswerNamesNoOtherTenantAndNoOtherProfile(t *testing.T) {
	broken := materialisedProfile("broken", alone("broken"))
	broken.Annotations[profilebundle.Annotation] = "!!! not base64"
	w := appResidueWorld(t, "multi", broken)
	// The cluster's list has reasons to be incomplete, and names them.
	cluster, _ := residueOf(t, w)
	said := strings.Join(cluster.Incomplete, "\n")
	if !strings.Contains(said, "rivalco") || !strings.Contains(said, "broken") {
		t.Fatalf("the cluster's list is not incomplete as arranged: %v", cluster.Incomplete)
	}

	got, err := w.svc.AppResidue(context.Background(), "demo", "shop")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	for _, word := range []string{"rivalco", `"user"`, "broken", "notes", "wiki", "gone", "odoo", "legacy", "drive", "companions", "catalogueNamespace"} {
		if strings.Contains(string(raw), word) {
			t.Errorf("the answer for shop in demo says %q: %s", word, raw)
		}
	}
	if len(got.Incomplete) != 0 {
		t.Fatalf("incomplete = %v", got.Incomplete)
	}

	// What cannot be established about the app itself is said.
	gift := materialisedProfile("shop-gift", alone("shop-gift"))
	gift.Annotations[profilebundle.Annotation] = "!!! not base64"
	w = appResidueWorld(t, "multi")
	if err := w.objs.Update(context.Background(), withVersionOf(t, w, gift)); err != nil {
		t.Fatal(err)
	}
	got, err = w.svc.AppResidue(context.Background(), "demo", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Incomplete) != 1 || !strings.Contains(got.Incomplete[0], "ComponentProfile shop-gift cannot be read") {
		t.Fatalf("incomplete = %v", got.Incomplete)
	}
	if names := namesOf(got.Residue); !reflect.DeepEqual(names, []string{"ConfigMap/shop.old-page", "OIDCPackCatalog/shop-old-oidc"}) {
		t.Fatalf("with the add-on's bundle unreadable: %v", names)
	}
}

func withVersionOf(t *testing.T, w *purgeWorld, p *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	var current gentianov1alpha1.ComponentProfile
	if err := w.objs.Get(context.Background(), client.ObjectKey{Name: p.Name}, &current); err != nil {
		t.Fatal(err)
	}
	p.ResourceVersion = current.ResourceVersion
	return p
}

// Who may remove is the server's to say, from the tenancy mode: the tenant's
// administrator only where the tenant is the one user tenant of a
// single-tenancy cluster, and the platform's everywhere else.
func TestWhoMayRemoveFollowsFromTheTenancyMode(t *testing.T) {
	for _, c := range []struct{ mode, tenant, want string }{
		{"single", "user", RemovableByTenant},
		{"SINGLE", "user", RemovableByTenant},
		{"multi", "user", RemovableByPlatform},
		{"", "user", RemovableByPlatform},
		{"nonsense", "user", RemovableByPlatform},
		{"multi", "demo", RemovableByPlatform},
		// Not the tenant such a cluster carries, whatever the mode says.
		{"single", "demo", RemovableByPlatform},
	} {
		w := appResidueWorld(t, c.mode)
		got, err := w.svc.AppResidue(context.Background(), c.tenant, "shop")
		if err != nil || got.RemovableBy != c.want {
			t.Errorf("mode %q, tenant %s: %+v, %v; want %s", c.mode, c.tenant, got, err, c.want)
		}
	}
}

func theUsers(profile string) ResidueScope {
	return ResidueScope{Tenant: gentianov1alpha1.SingleUserTenantName, Profile: profile}
}

// On a cluster with one user tenant its administrator's removal deletes a
// leftover of an app the tenant has, exactly as the cluster's removal would.
func TestATenantsRemovalDeletesALeftoverOfItsApp(t *testing.T) {
	w := appResidueWorld(t, "single")
	before := everything(t, w)
	ctx := context.Background()

	res, err := w.svc.RemoveAppResidue(ctx, theUsers("shop"), "ConfigMap", "shop.old-page", "", "uma@example.com")
	if err != nil || res.Status != "deleted" || res.Deleted == nil || res.Deleted.Name != "shop.old-page" ||
		!strings.Contains(res.Message, "uma@example.com") {
		t.Fatalf("answer = %+v, %v", res, err)
	}
	// An add-on's, asked for as the add-on's.
	if res, err := w.svc.RemoveAppResidue(ctx, theUsers("shop-gift"), "ConfigMap", "shop-gift.old", provisioningNS, "uma"); err != nil || res.Status != "deleted" {
		t.Fatalf("the add-on's: %+v, %v", res, err)
	}
	var want []string
	for _, name := range before {
		if name != "ConfigMap/"+provisioningNS+"/shop.old-page" && name != "ConfigMap/"+provisioningNS+"/shop-gift.old" {
			want = append(want, name)
		}
	}
	if after := everything(t, w); !reflect.DeepEqual(after, want) {
		t.Fatalf("after the removals:\n %v\nwant:\n %v", after, want)
	}
}

// Everything else is refused, and stays: another profile's leftover, what
// no bundle owns, a profile, what is not residue at all, and anything asked
// for an app the tenant does not have.
func TestATenantsRemovalRefusesEverythingButItsAppsLeftovers(t *testing.T) {
	w := appResidueWorld(t, "single")
	before := everything(t, w)

	for _, c := range []struct {
		scope                 ResidueScope
		kind, name, namespace string
		is                    error
		says                  string
	}{
		// Residue, and not this app's: another profile's, dropped and
		// orphaned; and the add-on's asked for as the app's.
		{theUsers("shop"), "Composition", "app-notes", "", ErrNotResidue, "not something a newer build of shop left behind"},
		{theUsers("shop"), "OIDCPackCatalog", "gone-oidc", "", ErrNotResidue, "not something a newer build of shop left behind"},
		{theUsers("shop"), "ConfigMap", "shop-gift.old", "", ErrNotResidue, "not something a newer build of shop left behind"},
		{theUsers("shop-gift"), "ConfigMap", "shop.old-page", "", ErrNotResidue, "not something a newer build of shop-gift left behind"},
		// Residue nobody owns: the cluster administrator's only.
		{theUsers("shop"), "Composition", "app-odoo", "", ErrNotResidue, "not something a newer build of shop"},
		{theUsers("shop"), "OIDCPackCatalog", "gentian-element-oidc", "", ErrNotResidue, "not something a newer build of shop"},
		{theUsers("shop"), "ConfigMap", "legacy.asset", "", ErrNotResidue, "not something a newer build of shop"},
		{theUsers("shop"), "Composition", "shop-composition", "", ErrNotResidue, "not something a newer build of shop"},
		// A profile: unused, and the app's own.
		{theUsers("shop"), "ComponentProfile", "notes", "", ErrNotResidue, "not removed for a tenant"},
		{theUsers("shop"), "ComponentProfile", "shop", "", ErrNotResidue, "not removed for a tenant"},
		// Not residue: what the app's bundle brings, and the platform's.
		{theUsers("shop"), "Composition", "app-shop", "", ErrNotResidue, "not something a newer build of shop"},
		{theUsers("shop"), "ConfigMap", "shop.theme", "", ErrNotResidue, "not something a newer build of shop"},
		{theUsers("shop"), "Composition", "app-default", "", ErrNotResidue, "not something a newer build of shop"},
		{theUsers("shop"), "ConfigMap", "nothing.here", "", ErrNotResidue, "not something a newer build of shop"},
		// The tenant's own namespace is never where a bundle's objects are.
		{theUsers("shop"), "ConfigMap", "shop.old-page", "tenant-user", ErrNotResidue, "nothing in another namespace"},
		{theUsers("shop"), "Secret", "keycloak-admin", "", ErrNotARemovableKind, "not a kind"},
		// An app the tenant does not have, and a tenant that is not there.
		{theUsers("notes"), "Composition", "app-notes", "", ErrNotInstalled, "notes in user"},
		{theUsers("drive"), "ConfigMap", "shop.old-page", "", ErrNotInstalled, "drive in user"},
		{theUsers("gone"), "OIDCPackCatalog", "gone-oidc", "", ErrNotInstalled, "gone in user"},
		{theUsers(""), "ConfigMap", "shop.old-page", "", ErrNotInstalled, ""},
	} {
		res, err := w.svc.RemoveAppResidue(context.Background(), c.scope, c.kind, c.name, c.namespace, "uma")
		if res != nil || !errors.Is(err, c.is) {
			t.Errorf("%s %s for %s: %+v, %v; want %v", c.kind, c.name, c.scope.Profile, res, err, c.is)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s %s is refused without saying %q: %v", c.kind, c.name, c.says, err)
		}
		// Why the cluster would not remove it can name another profile.
		if errors.Is(err, ErrNotResidue) && strings.Contains(err.Error(), "bundle now materialised") {
			t.Errorf("%s %s: the refusal says whose it is: %v", c.kind, c.name, err)
		}
	}
	if after := everything(t, w); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused removal deleted something:\n %v\nwas:\n %v", after, before)
	}
}

// Where the pieces are every tenant's, no tenant's removal deletes anything:
// under multi, and for any tenant but the one a single-tenancy cluster
// carries.
func TestATenantsRemovalIsRefusedWhereThePiecesAreShared(t *testing.T) {
	for _, c := range []struct{ mode, tenant string }{
		{"multi", "user"}, {"", "user"}, {"multi", "demo"}, {"single", "demo"}, {"single", "platform"},
	} {
		w := appResidueWorld(t, c.mode)
		before := everything(t, w)
		res, err := w.svc.RemoveAppResidue(context.Background(), ResidueScope{Tenant: c.tenant, Profile: "shop"},
			"ConfigMap", "shop.old-page", "", "uma")
		if res != nil || !errors.Is(err, ErrSharedResidue) {
			t.Errorf("mode %q, tenant %s: %+v, %v", c.mode, c.tenant, res, err)
		}
		if after := everything(t, w); !reflect.DeepEqual(after, before) {
			t.Errorf("mode %q, tenant %s: something was deleted", c.mode, c.tenant)
		}
	}
}

// A tenant's removal is the cluster's removal: what that refuses for its own
// reasons, this refuses the same way.
func TestATenantsRemovalPassesThroughTheClustersRefusals(t *testing.T) {
	w := appResidueWorld(t, "single", argoCatalogue("ConfigMap/shop.old-page"))
	_, err := w.svc.RemoveAppResidue(context.Background(), theUsers("shop"), "ConfigMap", "shop.old-page", "", "uma")
	if !errors.Is(err, ErrStillDeclared) || !strings.Contains(err.Error(), "gentian-catalogue-dev") {
		t.Fatalf("declared: %v", err)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("a declared object was deleted")
	}

	// Lost to something that touched the object after the check.
	w = appResidueWorld(t, "single")
	w.svc.client = touchBeforeDelete{Client: w.svc.client}
	_, err = w.svc.RemoveAppResidue(context.Background(), theUsers("shop"), "ConfigMap", "shop.old-page", "", "uma")
	if !errors.Is(err, ErrResidueChanged) {
		t.Fatalf("a lost race: %v", err)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("the object went although the removal lost")
	}

	// A new build that brings the piece again has adopted it since.
	w = appResidueWorld(t, "single")
	var shop gentianov1alpha1.ComponentProfile
	if err := w.objs.Get(context.Background(), client.ObjectKey{Name: "shop"}, &shop); err != nil {
		t.Fatal(err)
	}
	adopting := shopBundle + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop.old-page\n  labels:\n" +
		"    gentianos.io/profile-name: shop\n    gentianos.io/asset: old-page\ndata:\n  k: v\n"
	shop.Annotations[profilebundle.Annotation] = profilebundle.Encode([]byte(adopting))
	if err := w.objs.Update(context.Background(), &shop); err != nil {
		t.Fatal(err)
	}
	_, err = w.svc.RemoveAppResidue(context.Background(), theUsers("shop"), "ConfigMap", "shop.old-page", "", "uma")
	if !errors.Is(err, ErrNotResidue) {
		t.Fatalf("adopted: %v", err)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("an object a bundle brings was deleted")
	}
}

// The read is one the usher's identity is admitted to. The removal is the one
// command there is, with the tenant and the profile in its body, and half a
// scope is refused rather than read as none.
func TestTheAppResidueRoutes(t *testing.T) {
	w := appResidueWorld(t, "single")
	_, auth := cluster()
	mux := (&HTTPServer{Auth: auth, Service: w.svc}).routes()
	call := func(method, path, token, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("X-Gentian-Actor", "uma@example.com")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	for _, token := range []string{"usher", "director"} {
		code, out := call("GET", "/v1/tenants/user/apps/shop/residue", token, "")
		if list, _ := out["residue"].([]any); code != http.StatusOK || len(list) != 3 || out["removableBy"] != RemovableByTenant {
			t.Fatalf("the read as %s: %d %v", token, code, out)
		}
	}
	if code, out := call("GET", "/v1/tenants/demo/apps/shop/residue", "usher", ""); code != http.StatusOK || out["removableBy"] != RemovableByPlatform {
		t.Fatalf("another tenant of the same cluster: %d %v", code, out)
	}
	for _, path := range []string{"/v1/tenants/user/apps/notes/residue", "/v1/tenants/user/apps/absent/residue", "/v1/tenants/nobody/apps/shop/residue"} {
		if code, out := call("GET", path, "usher", ""); code != http.StatusNotFound || out["residue"] != nil {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	for _, token := range []string{"", "custodian"} {
		if code, _ := call("GET", "/v1/tenants/user/apps/shop/residue", token, ""); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("the read as %q: %d", token, code)
		}
	}

	remove := "/v1/actions/remove-catalogue-residue"
	// Half a scope.
	for _, body := range []string{
		`{"kind":"ConfigMap","name":"shop.old-page","tenant":"user"}`,
		`{"kind":"ConfigMap","name":"shop.old-page","profile":"shop"}`,
	} {
		if code, _ := call("POST", remove, "director", body); code != http.StatusBadRequest {
			t.Errorf("body %s: %d", body, code)
		}
	}
	if code, _ := call("POST", remove, "usher", `{"kind":"ConfigMap","name":"shop.old-page","tenant":"user","profile":"shop"}`); code != http.StatusForbidden {
		t.Fatalf("the usher removed something: %d", code)
	}
	// Another tenant of the cluster, which is not the one it carries.
	code, out := call("POST", remove, "director", `{"kind":"ConfigMap","name":"shop.old-page","tenant":"demo","profile":"shop"}`)
	if code != http.StatusForbidden || out["reason"] != RefusedShared || out["detail"] != tenancy.SharedPieces {
		t.Fatalf("a shared piece: %d %v", code, out)
	}
	if code, _ := call("POST", remove, "director", `{"kind":"Composition","name":"app-notes","tenant":"user","profile":"notes"}`); code != http.StatusNotFound {
		t.Fatalf("an app the tenant does not have: %d", code)
	}
	code, out = call("POST", remove, "director", `{"kind":"Composition","name":"app-odoo","tenant":"user","profile":"shop"}`)
	if code != http.StatusConflict || out["reason"] != RefusedNotResidue || !strings.Contains(out["detail"].(string), "Nothing was deleted") {
		t.Fatalf("what nobody owns: %d %v", code, out)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") || !exists(t, w, "apiextensions.crossplane.io/v1", "Composition", "", "app-odoo") {
		t.Fatal("a refused request deleted something")
	}
	code, out = call("POST", remove, "director", `{"kind":"ConfigMap","name":"shop.old-page","tenant":"user","profile":"shop"}`)
	if deleted, _ := out["deleted"].(map[string]any); code != http.StatusOK || out["status"] != "deleted" || deleted["name"] != "shop.old-page" {
		t.Fatalf("the removal: %d %v", code, out)
	}
	if exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("answered deleted, and it is there")
	}
	// The cluster's removal is what it was: no scope, anything on the list.
	if code, out := call("POST", remove, "director", `{"kind":"Composition","name":"app-odoo"}`); code != http.StatusOK || out["status"] != "deleted" {
		t.Fatalf("the cluster's removal: %d %v", code, out)
	}
}
