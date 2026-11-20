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

// store answers reads from a list of tuples as OpenFGA's read does: a filter
// field that is set must match, and an object that ends in ':' names a type.
func store(tuples ...Tuple) readFunc {
	return func(_ context.Context, f Tuple) ([]Tuple, error) {
		var out []Tuple
		for _, t := range tuples {
			if f.User != "" && f.User != t.User {
				continue
			}
			if f.Relation != "" && f.Relation != t.Relation {
				continue
			}
			if strings.HasSuffix(f.Object, ":") {
				if !strings.HasPrefix(t.Object, f.Object) {
					continue
				}
			} else if f.Object != "" && f.Object != t.Object {
				continue
			}
			out = append(out, t)
		}
		return out, nil
	}
}

// answering says yes to the questions listed as "user relation object", and
// records every question asked.
func answering(asked *[]Question, yes ...string) askFunc {
	return func(_ context.Context, questions []Question) ([]bool, error) {
		out := make([]bool, len(questions))
		for i, q := range questions {
			*asked = append(*asked, q)
			for _, y := range yes {
				if y == q.User+" "+q.Relation+" "+q.Object {
					out[i] = true
				}
			}
		}
		return out, nil
	}
}

func permission(t *testing.T, access ObjectAccess, name string) Permission {
	t.Helper()
	for _, p := range access.Permissions {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no permission %s on %s: %+v", name, access.Object, access.Permissions)
	return Permission{}
}

var demo = []Tuple{
	{User: "user:anna", Relation: "member", Object: "group:gentian/tenant/demo/members"},
	{User: "user:anna", Relation: "member", Object: "group:gentian/tenant/demo/app/notes"},
	{User: "user:anna", Relation: "member", Object: "group:gentian/tenant/demo/sales"},
	{User: "user:ben", Relation: "member", Object: "group:gentian/tenant/demo/admins"},
	{User: "user:ben", Relation: "member", Object: "group:gentian/tenant/demo/app/notes"},
	{User: "cluster:c1", Relation: "cluster", Object: "tenant:demo"},
	{User: "group:gentian/tenant/demo/admins#member", Relation: "admin", Object: "tenant:demo"},
	{User: "group:gentian/tenant/demo/members#member", Relation: "member", Object: "tenant:demo"},
	{User: "user:demo/cara", Relation: "auditor", Object: "tenant:demo"},
	{User: "tenant:demo", Relation: "tenant", Object: "app:demo/notes"},
	{User: "tenant:demo", Relation: "tenant", Object: "app:demo/wiki"},
	{User: "tenant:other", Relation: "tenant", Object: "app:other/notes"},
	{User: "group:gentian/tenant/demo/app/notes#member", Relation: "entitled", Object: "app:demo/notes"},
	{User: "group:gentian/tenant/other/app/notes#member", Relation: "entitled", Object: "app:other/notes"},
	{User: "user:platform/dora", Relation: "auditor", Object: "cluster:c1"},
}

// A member's rights are traced to the groups they are held through, and every
// app of the tenant is listed, also the one they hold nothing on.
func TestAPersonsAccessNamesTheGroupEachRoleIsHeldThrough(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:anna member tenant:demo", "user:anna can_enter tenant:demo", "user:anna can_view tenant:demo",
		"user:anna can_use app:demo/notes", "user:anna can_launch app:demo/notes",
	), "c1", "demo", "anna")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gentian:tenant:demo:app:notes", "gentian:tenant:demo:members", "gentian:tenant:demo:sales"}
	if !reflect.DeepEqual(got.Groups, want) {
		t.Fatalf("groups = %v", got.Groups)
	}
	if len(got.OnTenant.Held) != 1 || got.OnTenant.Held[0].Role != "member" ||
		got.OnTenant.Held[0].Group != "gentian:tenant:demo:members" || got.OnTenant.Held[0].ByName {
		t.Fatalf("held on the tenant = %+v", got.OnTenant.Held)
	}
	if strings.Join(got.OnTenant.Held[0].Carries, ",") != "can_enter,can_view" {
		t.Fatalf("member carries %v", got.OnTenant.Held[0].Carries)
	}
	if p := permission(t, got.OnTenant, "can_enter"); !p.InEffect || p.From != FromRole {
		t.Fatalf("can_enter = %+v", p)
	}
	if p := permission(t, got.OnTenant, "can_manage_users"); p.InEffect || p.From != "" {
		t.Fatalf("can_manage_users = %+v", p)
	}
	if len(got.Apps) != 2 || got.Apps[0].App != "notes" || got.Apps[1].App != "wiki" {
		t.Fatalf("apps = %+v", got.Apps)
	}
	notes := got.Apps[0]
	if len(notes.Held) != 1 || notes.Held[0].Role != "entitled" || notes.Held[0].Group != "gentian:tenant:demo:app:notes" {
		t.Fatalf("held on notes = %+v", notes.Held)
	}
	if p := permission(t, notes, "can_use"); !p.InEffect || p.From != FromRole {
		t.Fatalf("can_use on notes = %+v", p)
	}
	if len(got.Apps[1].Held) != 0 {
		t.Fatalf("held on wiki = %+v", got.Apps[1].Held)
	}
	// Nothing is asked about another tenant's objects.
	for _, q := range asked {
		if strings.Contains(q.Object, "other") {
			t.Fatalf("asked about %s", q.Object)
		}
	}
}

// The two ways the roles and the store's answer disagree are both said: an
// app's use is carried by entitled and withheld from an administrator, and an
// app's credentials follow from being the tenant's administrator.
func TestWhatIsWithheldAndWhatFollowsFromTheTenantIsSaid(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:ben admin tenant:demo", "user:ben can_administer tenant:demo", "user:ben can_read_credential tenant:demo",
		"user:ben can_write_credential app:demo/notes", "user:ben can_read_credential app:demo/notes",
	), "c1", "demo", "ben")
	if err != nil {
		t.Fatal(err)
	}
	notes := got.Apps[0]
	if p := permission(t, notes, "can_use"); p.InEffect || p.From != FromRole {
		t.Fatalf("can_use, carried and withheld = %+v", p)
	}
	if p := permission(t, notes, "can_write_credential"); !p.InEffect || p.From != FromTenant {
		t.Fatalf("can_write_credential = %+v", p)
	}
	if p := permission(t, notes, "can_read_credential"); !p.InEffect || p.From != FromTenant {
		t.Fatalf("can_read_credential = %+v", p)
	}
	if p := permission(t, notes, "can_administer"); p.InEffect || p.From != "" {
		t.Fatalf("can_administer = %+v", p)
	}
}

// A role held by name is stored under the realm, and asked as that user too.
func TestARoleHeldByNameIsFoundAndAskedAsItsHolder(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:demo/cara can_read_evidence tenant:demo",
	), "c1", "demo", "cara")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OnTenant.Held) != 1 || got.OnTenant.Held[0].Role != "auditor" || !got.OnTenant.Held[0].ByName {
		t.Fatalf("held = %+v", got.OnTenant.Held)
	}
	if p := permission(t, got.OnTenant, "can_read_evidence"); !p.InEffect || p.From != FromRole {
		t.Fatalf("can_read_evidence = %+v", p)
	}
}

// What follows from the cluster is in effect and not traced: it is said to
// come from elsewhere, and the cluster's own role is not listed as the
// tenant's.
func TestWhatFollowsFromTheClusterIsSaidToComeFromElsewhere(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:platform/dora can_view tenant:demo",
	), "c1", "demo", "dora")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OnTenant.Held) != 0 {
		t.Fatalf("held = %+v", got.OnTenant.Held)
	}
	if p := permission(t, got.OnTenant, "can_view"); !p.InEffect || p.From != FromElsewhere {
		t.Fatalf("can_view = %+v", p)
	}
	// Without the cluster's id the name is not found, and nothing is asked
	// as it.
	asked = nil
	if _, err := personAccess(context.Background(), store(demo...), answering(&asked), "", "demo", "dora"); err != nil {
		t.Fatal(err)
	}
	for _, q := range asked {
		if q.User != "user:dora" {
			t.Fatalf("asked as %s", q.User)
		}
	}
}

func TestAPersonsAccessFailsWhenTheStoreDoes(t *testing.T) {
	down := errors.New("down")
	if _, err := personAccess(context.Background(),
		func(context.Context, Tuple) ([]Tuple, error) { return nil, down },
		answering(new([]Question)), "c1", "demo", "anna"); !errors.Is(err, down) {
		t.Fatalf("err = %v", err)
	}
	if _, err := personAccess(context.Background(), store(demo...),
		func(context.Context, []Question) ([]bool, error) { return nil, down }, "c1", "demo", "anna"); !errors.Is(err, down) {
		t.Fatalf("err = %v", err)
	}
	if _, err := personAccess(context.Background(), store(demo...), answering(new([]Question)), "c1", "demo", "a/b"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("err = %v", err)
	}
}

func TestAGroupsAccessIsWhatItHoldsInTheTenant(t *testing.T) {
	got, err := groupAccess(context.Background(), store(demo...), "demo", "gentian:tenant:demo:app:notes")
	if err != nil {
		t.Fatal(err)
	}
	want := []GroupHold{{Object: "app:demo/notes", App: "notes", Role: "entitled", Carries: []string{"can_launch", "can_use"}}}
	if !reflect.DeepEqual(got.Holds, want) {
		t.Fatalf("holds = %+v", got.Holds)
	}
	admins, err := groupAccess(context.Background(), store(demo...), "demo", "gentian:tenant:demo:admins")
	if err != nil {
		t.Fatal(err)
	}
	if len(admins.Holds) != 1 || admins.Holds[0].Object != "tenant:demo" || admins.Holds[0].Role != "admin" {
		t.Fatalf("holds = %+v", admins.Holds)
	}
	// A group that holds nothing gives nothing, and another tenant's objects
	// are not this tenant's to show.
	for _, group := range []string{"gentian:tenant:demo:sales", "gentian:tenant:other:app:notes"} {
		got, err := groupAccess(context.Background(), store(demo...), "demo", group)
		if err != nil || len(got.Holds) != 0 {
			t.Fatalf("%s: holds = %+v, err = %v", group, got.Holds, err)
		}
	}
}
