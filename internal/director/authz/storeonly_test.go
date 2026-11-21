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
	"sort"
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
	standing, once := tenantDefaults(cluster, tenant)
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
	declared, err := positionTuples(tenant, positionsFor(apps))
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range declared {
		store[tuple] = true
	}
	return store
}

// positionsFor is the positions a tenant of these tests has: the three
// every tenant has, and one of its own.
func positionsFor(apps []InstalledApp) []PositionSpec {
	return append(ShippedPositions(apps), PositionSpec{Name: "leads", Roles: []RoleGrant{{Role: RoleUser}}})
}

// What the projection writes is what a backup leaves out: the two are told
// apart by one list (tenantDefaults, appTuples, positionTuples), so a tenant that holds
// exactly the projection's entries has nothing only the store knows --
// whatever the projection comes to write.
func TestWhatTheProjectionWritesIsNotStoreOnly(t *testing.T) {
	apps := []InstalledApp{{Profile: "wiki", ForEveryone: true}, {Profile: "office", Addons: []InstalledApp{
		{Profile: "sheets", Roles: []DeclaredRole{{Name: "auditor"}}}, {Profile: "slides"},
	}}}
	positions := positionsFor(apps)
	store := projection(t, "c1", "demo", apps)
	// Another tenant's entries, and a group of the cluster's, are nobody's
	// here.
	for tuple := range projection(t, "c1", "other", apps) {
		store[tuple] = true
	}
	store[Tuple{User: "user:42", Relation: "member", Object: "group:gentian/platform/admin"}] = true
	store[Tuple{User: "user:77", Relation: "holder", Object: "position:other/leads"}] = true

	got, err := StoreOnlyOf(context.Background(), store, "c1", "demo", apps, positions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Granted) != 0 || len(got.Withdrawn) != 0 {
		t.Fatalf("a tenant with the projection's entries only: %+v", got)
	}

	// Who holds a position, and a role given beside one: on the tenant, on
	// an app, on a role an add-on declares, on an app that is no longer
	// installed. One default less.
	extra := []Tuple{
		{User: "user:42", Relation: "admin", Object: "app:demo/notes"},
		{User: "user:42", Relation: "admin", Object: "app:demo/wiki"},
		{User: "user:42", Relation: "holder", Object: "position:demo/leads"},
		{User: "user:43", Relation: "holder", Object: "position:demo/tenant-administrator"},
		{User: "user:44", Relation: "holder", Object: "role:demo/sheets/auditor"},
		{User: "user:45", Relation: "admin", Object: "tenant:demo"},
	}
	for _, tuple := range extra {
		store[tuple] = true
	}
	delete(store, Tuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"})
	// What the projections are about to remove is not a right anybody
	// holds: a role a position no longer holds, a position that is no longer
	// declared and who held it, an add-on that is gone.
	for _, going := range []Tuple{
		{User: "position:demo/leads#holder", Relation: "admin", Object: "app:demo/wiki"},
		{User: "position:demo/old#holder", Relation: "user", Object: "tenant:demo"},
		{User: "user:46", Relation: "holder", Object: "position:demo/old"},
		{User: "app:demo/gone", Relation: "addon", Object: "app:demo/wiki"},
	} {
		store[going] = true
	}

	got, err = StoreOnlyOf(context.Background(), store, "c1", "demo", apps, positions, []string{"notes", "bad name", "a/b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Tuple{extra[0], extra[1], extra[2], extra[3], extra[4], extra[5]}
	sort.Slice(want, func(i, j int) bool { return key(want[i]) < key(want[j]) })
	if !reflect.DeepEqual(got.Granted, want) {
		t.Errorf("granted = %v\nwant %v", got.Granted, want)
	}
	if !reflect.DeepEqual(got.Withdrawn, []Tuple{{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"}}) {
		t.Errorf("withdrawn = %v", got.Withdrawn)
	}
	// Without the app being named as held, its object is not read: nothing
	// attaches an uninstalled app to its tenant.
	got, _ = StoreOnlyOf(context.Background(), store, "c1", "demo", apps, positions, nil)
	if len(got.Granted) != 5 {
		t.Errorf("without the uninstalled app named: %v", got.Granted)
	}

	// Who approves the tenant's perimeter is the Cluster claim's to say and
	// the projection's to write: never a right only the store knows, so a
	// bundle does not carry it.
	// Nor is who reads its evidence.
	for _, held := range []Tuple{
		{User: "user:demo/tom", Relation: "perimeter_approver", Object: "tenant:demo"},
		{User: "user:kernel/dana", Relation: "auditor", Object: "tenant:demo"},
	} {
		store[held] = true
		got, _ = StoreOnlyOf(context.Background(), store, "c1", "demo", apps, positions, nil)
		for _, tuple := range got.Granted {
			if tuple == held {
				t.Errorf("%s travels as a store-only right: %v", held.Relation, got.Granted)
			}
		}
		delete(store, held)
	}

	// The platform tenant is always operated by its cluster.
	platform := projection(t, "c1", PlatformTenant, nil)
	delete(platform, Tuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:" + PlatformTenant})
	if got, err := StoreOnlyOf(context.Background(), platform, "c1", PlatformTenant, nil, positionsFor(nil), nil); err != nil || len(got.Withdrawn) != 0 {
		t.Errorf("the platform tenant: %+v, %v", got, err)
	}
	// A tenant the projection has not attached has nothing withdrawn.
	if _, err := StoreOnlyOf(context.Background(), memoryStore{}, "c1", "demo", nil, nil, nil); !errors.Is(err, ErrNotProjected) {
		t.Errorf("an unattached tenant: %v", err)
	}
}

// An entry of one cluster under the names of another: the cluster, wherever
// it stands; nothing else, the tenant least of all.
func TestRebase(t *testing.T) {
	cases := []struct{ in, want Tuple }{
		{Tuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"}, Tuple{User: "cluster:c2", Relation: "operated_by", Object: "tenant:demo"}},
		{Tuple{User: "cluster:c1#operator", Relation: "admin", Object: "app:demo/wiki"}, Tuple{User: "cluster:c2#operator", Relation: "admin", Object: "app:demo/wiki"}},
		{Tuple{User: "user:42", Relation: "holder", Object: "position:demo/leads"}, Tuple{User: "user:42", Relation: "holder", Object: "position:demo/leads"}},
		{Tuple{User: "cluster:c10", Relation: "operated_by", Object: "tenant:demo"}, Tuple{User: "cluster:c10", Relation: "operated_by", Object: "tenant:demo"}},
	}
	for _, c := range cases {
		if got := Rebase(c.in, "c1", "c2"); got != c.want {
			t.Errorf("Rebase(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	same := Tuple{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"}
	if got := Rebase(same, "c1", "c1"); got != same {
		t.Errorf("on the same cluster: %v", got)
	}
	for object, on := range map[string]bool{
		"tenant:acme": true, "app:acme/wiki": true, "role:acme/wiki/editor": true, "position:acme/leads": true,
		"app:acme/": false, "position:acme/": false, "tenant:acme2": false, "app:acme2/wiki": false, "position:acme2/leads": false,
		"cluster:c1": false, "group:gentian/platform/admin": false,
	} {
		if got := OnTenant(Tuple{Object: object}, "acme"); got != on {
			t.Errorf("OnTenant(%s) = %v", object, got)
		}
	}
}

// An entry from a bundle is written with the store's key: one that is no
// entry of the store's is told from one that is.
func TestWellFormed(t *testing.T) {
	good := []Tuple{
		{User: "position:demo/tenant-administrator#holder", Relation: "admin", Object: "tenant:demo"},
		{User: "user:42", Relation: "holder", Object: "role:demo/wiki/editor"},
		{User: "cluster:c1", Relation: "operated_by", Object: "tenant:demo"},
		{User: "tenant:demo", Relation: "tenant", Object: "app:demo/wiki"},
	}
	for _, tuple := range good {
		if err := WellFormed(tuple); err != nil {
			t.Errorf("%v: %v", tuple, err)
		}
	}
	bad := []Tuple{
		{User: "", Relation: "admin", Object: "tenant:demo"}, {User: "user:*", Relation: "admin", Object: "tenant:demo"}, {User: "group:x#", Relation: "admin", Object: "tenant:demo"},
		{User: "group:x#member", Relation: "", Object: "tenant:demo"}, {User: "group:x#member", Relation: "admin", Object: "tenant:demo#member"},
		{User: "group:x#member", Relation: "admin", Object: "tenant"}, {User: "group:x#member", Relation: "ad min", Object: "tenant:demo"},
		{User: "group:x #member", Relation: "admin", Object: "tenant:demo"}, {User: "group:x#member", Relation: "admin", Object: "tenant:de:mo"},
	}
	for _, tuple := range bad {
		if WellFormed(tuple) == nil {
			t.Errorf("%v is taken for an entry of the store", tuple)
		}
	}
}
