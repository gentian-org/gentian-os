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
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

func retainedByProfile(apps []RetainedApp) map[string]RetainedApp {
	out := map[string]RetainedApp{}
	for _, a := range apps {
		out[a.Profile] = a
	}
	return out
}

// The read lists an uninstalled app that still has a database, with each
// kind of data it holds, and leaves out everything that is not "uninstalled,
// data retained": an installed app with exactly the same stores, the
// desktop's own store, and an app whose release is still being uninstalled.
// It is open to the usher, and it changes nothing.
func TestTheRetainedReadListsAnUninstalledAppAndOmitsAnInstalledOne(t *testing.T) {
	tenant := demoTenant()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "drive"}}
	w := newPurgeWorld(t, tenant,
		[]client.Object{
			wikiProfile(),
			component("drive", "Ready", "", true),
			// The desktop's database is recorded like an app's.
			databaseRecord("demo", "shell"),
			// Uninstalled a moment ago: its release is still going.
			databaseRecord("demo", "notes"),
		},
		helmRecord("notes-release"),
	)
	w.vault.paths["gentian-os/tenants/demo/apps/shell/database"] = true
	w.kube.ClearActions()

	_, auth := cluster()
	h := &HTTPServer{Auth: auth, Service: w.svc}
	r := httptest.NewRequest("GET", "/v1/tenants/demo/apps/retained", nil)
	r.Header.Set("Authorization", "Bearer usher")
	rec := httptest.NewRecorder()
	h.routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("the usher's read answered %d: %s", rec.Code, rec.Body)
	}
	var got RetainedApps
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Tenant != "demo" || len(got.Apps) != 1 {
		t.Fatalf("apps = %+v, want wiki alone", got.Apps)
	}
	wiki := got.Apps[0]
	if wiki.Profile != "wiki" || wiki.State != "retained" || !wiki.ProfileAvailable {
		t.Fatalf("wiki = %+v", wiki)
	}
	want := map[string]string{
		KindDatabase:    RetainedPresent,
		KindFiles:       RetainedPresent,
		KindCredentials: RetainedPresent,
		KindAccessGroup: RetainedPresent,
		// Declared by the profile, and only the store itself could say.
		KindObjectStorage: RetainedUnknown,
		KindCache:         RetainedUnknown,
	}
	if !reflect.DeepEqual(wiki.Kinds, want) {
		t.Errorf("kinds = %v, want %v", wiki.Kinds, want)
	}
	// The claims a purge would delete: the app's own and its extension's,
	// and not the installed app's.
	if want := []string{"wiki-mcp-release-cache", "wiki-release-data"}; !reflect.DeepEqual(wiki.Volumes, want) {
		t.Errorf("volumes = %v, want %v", wiki.Volumes, want)
	}
	for _, kind := range []string{KindObjectStorage, KindCache} {
		if got.Unknown[kind] == "" {
			t.Errorf("the answer does not say why %s is unknown", kind)
		}
	}

	// A read: nothing created, deleted or run, and no credential deleted.
	for _, action := range w.kube.Actions() {
		if verb := action.GetVerb(); verb != "list" && verb != "get" {
			t.Errorf("the read did %s %s", verb, action.GetResource().Resource)
		}
	}
	if len(w.vault.deleted)+len(w.jobs)+len(w.sql)+w.groups.deletes != 0 {
		t.Errorf("the read changed something: vault=%v jobs=%v sql=%v", w.vault.deleted, w.jobs, w.sql)
	}

	// And once the app is purged it is no longer listed.
	if _, err := w.svc.PurgeApp(context.Background(), "demo", "wiki", "tom"); err != nil {
		t.Fatal(err)
	}
	after, err := w.svc.RetainedApps(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Apps) != 0 {
		t.Errorf("after its purge the app is still listed: %+v", after.Apps)
	}
}

// What each kind reports follows the app's profile and what could be asked.
func TestTheRetainedReadSaysPresentAbsentOrUnknownPerKind(t *testing.T) {
	plain := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "notes"}}
	maria := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: gentianov1alpha1.ComponentProfileSpec{Requires: &gentianov1alpha1.RequirementSpec{
			Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEngineMariaDB}}}},
	}
	w := newPurgeWorld(t, nil, []client.Object{plain, maria},
		// A claim a StatefulSet made for an app whose profile is gone.
		runtime.Object(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "data-ghost-release-0", Namespace: "tenant-demo",
			Labels: map[string]string{"app.kubernetes.io/instance": "ghost-release"}}}),
	)
	w.groups.members[keycloak.TenantAppGroup("demo", "notes")] = nil
	w.groups.members[keycloak.TenantAppGroup("demo", "shop")] = nil

	got, err := w.svc.RetainedApps(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	apps := retainedByProfile(got.Apps)

	// A profile that declares no store: only what is there is present, and
	// the rest is known to be absent.
	notes := apps["notes"]
	for kind, want := range map[string]string{
		KindAccessGroup: RetainedPresent, KindDatabase: RetainedAbsent, KindObjectStorage: RetainedAbsent,
		KindCache: RetainedAbsent, KindFiles: RetainedAbsent, KindCredentials: RetainedAbsent,
	} {
		if notes.Kinds[kind] != want {
			t.Errorf("notes: %s = %q, want %q", kind, notes.Kinds[kind], want)
		}
	}
	// A MariaDB database is not recorded on the cluster.
	if got := apps["shop"].Kinds[KindDatabase]; got != RetainedUnknown {
		t.Errorf("shop: database = %q, want unknown", got)
	}
	// No profile: what it declared is not known.
	ghost := apps["ghost"]
	if ghost.ProfileAvailable || ghost.Kinds[KindFiles] != RetainedPresent ||
		ghost.Kinds[KindObjectStorage] != RetainedUnknown || ghost.Kinds[KindCache] != RetainedUnknown {
		t.Errorf("ghost = %+v", ghost)
	}
	// wiki's profile is gone here too, so its extension's vault path is not
	// known to be its own and is reported under the name a purge would need.
	if _, ok := apps["wiki-mcp"]; !ok {
		t.Errorf("the extension path of an app with no profile is not reported: %v", got.Apps)
	}
}

// A place that cannot be asked does not fail the read or hide what the
// others report: its kind is unknown, and the answer says why.
func TestTheRetainedReadGoesOnWhenASourceCannotBeAsked(t *testing.T) {
	w := newPurgeWorld(t, nil, []client.Object{wikiProfile()})
	w.vault.down = true
	w.groups.fail = true

	got, err := w.svc.RetainedApps(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Apps) != 1 || got.Apps[0].Profile != "wiki" {
		t.Fatalf("apps = %+v", got.Apps)
	}
	kinds := got.Apps[0].Kinds
	if kinds[KindDatabase] != RetainedPresent || kinds[KindCredentials] != RetainedUnknown || kinds[KindAccessGroup] != RetainedUnknown {
		t.Errorf("kinds = %v", kinds)
	}
	if got.Unknown[KindCredentials] == "" || got.Unknown[KindAccessGroup] == "" {
		t.Errorf("the answer does not say what could not be asked: %v", got.Unknown)
	}

	// The same with no vault configured at all.
	w.svc.vault = nil
	got, err = w.svc.RetainedApps(context.Background(), "demo")
	if err != nil || got.Unknown[KindCredentials] == "" {
		t.Fatalf("unknown = %v, err = %v", got.Unknown, err)
	}
}
