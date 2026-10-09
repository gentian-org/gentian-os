/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// memoryStore is the store as Read answers: an entry matches a filter field
// that is empty or equal, and an object filter that is a bare type.
type memoryStore map[Tuple]bool

func (m memoryStore) Read(_ context.Context, f Tuple) ([]Tuple, error) {
	var out []Tuple
	for t := range m {
		if (f.User == "" || f.User == t.User) && (f.Relation == "" || f.Relation == t.Relation) &&
			(f.Object == "" || f.Object == t.Object || (strings.HasSuffix(f.Object, ":") && strings.HasPrefix(t.Object, f.Object))) {
			out = append(out, t)
		}
	}
	return out, nil
}

func projection(t *testing.T, cluster, tenant string, apps []InstalledApp) memoryStore {
	t.Helper()
	store := memoryStore{}
	standing, once, err := tenantDefaults(cluster, tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range append(standing, once...) {
		store[tuple] = true
	}
	want, err := appTuples(tenant, apps)
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range want {
		store[tuple] = true
	}
	return store
}

// What the projection writes is what a backup leaves out: the two are told
// apart by one list (tenantDefaults, appTuples), so a tenant that holds
// exactly the projection's entries has nothing only the store knows --
// whatever the projection comes to write.
func TestWhatTheProjectionWritesIsNotStoreOnly(t *testing.T) {
	apps := []InstalledApp{{Profile: "wiki"}, {Profile: "office", Addons: []string{"sheets", "slides"}}}
	store := projection(t, "c1", "demo", apps)
	// Another tenant's entries, and memberships, are nobody's here.
	for tuple := range projection(t, "c1", "other", apps) {
		store[tuple] = true
	}
	store[Tuple{User: "user:42", Relation: "member", Object: "group:gentian/tenant/demo/admins"}] = true

	got, err := StoreOnlyOf(context.Background(), store, "c1", "demo", apps, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Granted) != 0 || len(got.Withdrawn) != 0 {
		t.Fatalf("a tenant with the projection's entries only: %+v", got)
	}

	// One entry more on the tenant, one on an app, one on an app that is no
	// longer installed; one default less.
	extra := []Tuple{
		{User: "group:gentian/platform/support#member", Relation: "admin", Object: "tenant:demo"},
		{User: "group:gentian/tenant/demo/leads#member", Relation: "admin", Object: "app:demo/wiki"},
		{User: "group:gentian/tenant/demo/leads#member", Relation: "admin", Object: "app:demo/notes"},
	}
	for _, tuple := range extra {
		store[tuple] = true
	}
	delete(store, Tuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"})
	// What the projection is about to remove is not a right anybody holds.
	store[Tuple{User: "group:gentian/tenant/demo/app/old#member", Relation: "entitled", Object: "app:demo/wiki"}] = true

	got, err = StoreOnlyOf(context.Background(), store, "c1", "demo", apps, []string{"notes", "bad name", "a/b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Tuple{extra[0], extra[2], extra[1]}
	if !reflect.DeepEqual(got.Granted, want) {
		t.Errorf("granted = %v\nwant %v", got.Granted, want)
	}
	if !reflect.DeepEqual(got.Withdrawn, []Tuple{{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"}}) {
		t.Errorf("withdrawn = %v", got.Withdrawn)
	}
	// Without the app being named as held, its object is not read: nothing
	// attaches an uninstalled app to its tenant.
	got, _ = StoreOnlyOf(context.Background(), store, "c1", "demo", apps, nil)
	if len(got.Granted) != 2 {
		t.Errorf("without the uninstalled app named: %v", got.Granted)
	}

	// The platform tenant is always operated by its cluster.
	platform := projection(t, "c1", PlatformTenant, nil)
	delete(platform, Tuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:" + PlatformTenant})
	if got, err := StoreOnlyOf(context.Background(), platform, "c1", PlatformTenant, nil, nil); err != nil || len(got.Withdrawn) != 0 {
		t.Errorf("the platform tenant: %+v, %v", got, err)
	}
	// A tenant the projection has not attached has nothing withdrawn.
	if _, err := StoreOnlyOf(context.Background(), memoryStore{}, "c1", "demo", nil, nil); !errors.Is(err, ErrNotProjected) {
		t.Errorf("an unattached tenant: %v", err)
	}
}

// An entry of one tenant under the names of another: the tenant, its apps,
// its groups and the cluster, wherever they stand; nothing else.
func TestRebase(t *testing.T) {
	cases := []struct{ in, want Tuple }{
		{Tuple{"cluster:c1", "operated_by", "tenant:demo"}, Tuple{"cluster:c2", "operated_by", "tenant:acme"}},
		{Tuple{"group:gentian/tenant/demo/leads#member", "admin", "app:demo/wiki"}, Tuple{"group:gentian/tenant/acme/leads#member", "admin", "app:acme/wiki"}},
		{Tuple{"group:gentian/platform/support#member", "admin", "tenant:demo"}, Tuple{"group:gentian/platform/support#member", "admin", "tenant:acme"}},
		// A name that only begins with the tenant's is another tenant's.
		{Tuple{"group:gentian/tenant/demo2/admins#member", "admin", "tenant:demo2"}, Tuple{"group:gentian/tenant/demo2/admins#member", "admin", "tenant:demo2"}},
		{Tuple{"tenant:demo", "tenant", "app:demo2/wiki"}, Tuple{"tenant:acme", "tenant", "app:demo2/wiki"}},
	}
	for _, c := range cases {
		if got := Rebase(c.in, "demo", "acme", "c1", "c2"); got != c.want {
			t.Errorf("Rebase(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	same := Tuple{"cluster:c1", "operated_by", "tenant:demo"}
	if got := Rebase(same, "demo", "demo", "c1", "c1"); got != same {
		t.Errorf("into the same tenant: %v", got)
	}
	for object, on := range map[string]bool{"tenant:acme": true, "app:acme/wiki": true, "app:acme/": false, "tenant:acme2": false, "app:acme2/wiki": false, "cluster:c1": false, "group:gentian/tenant/acme/admins": false} {
		if got := OnTenant(Tuple{Object: object}, "acme"); got != on {
			t.Errorf("OnTenant(%s) = %v", object, got)
		}
	}
}

// An entry from a bundle is written with the store's key: one that is no
// entry of the store's is told from one that is.
func TestWellFormed(t *testing.T) {
	good := []Tuple{
		{"group:gentian/tenant/demo/admins#member", "admin", "tenant:demo"},
		{"cluster:c1", "operated_by", "tenant:demo"},
		{"tenant:demo", "tenant", "app:demo/wiki"},
	}
	for _, tuple := range good {
		if err := WellFormed(tuple); err != nil {
			t.Errorf("%v: %v", tuple, err)
		}
	}
	bad := []Tuple{
		{"", "admin", "tenant:demo"}, {"user:*", "admin", "tenant:demo"}, {"group:x#", "admin", "tenant:demo"},
		{"group:x#member", "", "tenant:demo"}, {"group:x#member", "admin", "tenant:demo#member"},
		{"group:x#member", "admin", "tenant"}, {"group:x#member", "ad min", "tenant:demo"},
		{"group:x #member", "admin", "tenant:demo"}, {"group:x#member", "admin", "tenant:de:mo"},
	}
	for _, tuple := range bad {
		if WellFormed(tuple) == nil {
			t.Errorf("%v is taken for an entry of the store", tuple)
		}
	}
}
