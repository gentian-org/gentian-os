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
	"strings"
	"testing"
)

func reader(have map[string][]string) func(context.Context, Tuple) ([]Tuple, error) {
	return func(_ context.Context, f Tuple) ([]Tuple, error) {
		var out []Tuple
		for _, u := range have[f.Relation] {
			out = append(out, Tuple{User: u, Relation: f.Relation, Object: f.Object})
		}
		return out, nil
	}
}

// The view answers both halves. Tuples alone say who holds "admin"; what
// admin lets them do is several derivations into the model, and nobody should
// have to read model.fga to find out.
func TestTheViewResolvesWhoHoldsWhatAndWhatItCarries(t *testing.T) {
	v, err := viewOf(context.Background(), reader(map[string][]string{
		"admin":   {"group:gentian/platform/admins#member"},
		"auditor": {"group:gentian/platform/auditors#member"},
	}), "cluster:c1")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Binding{}
	for _, b := range v.Bindings {
		by[b.Relation] = b
	}
	admin, ok := by["admin"]
	if !ok {
		t.Fatalf("no admin binding in %+v", v.Bindings)
	}
	// A group of the sign-in service, in its spelling there: the ids differ
	// only in the separator, and the person reading this screen knows that
	// name.
	if len(admin.Holders) != 1 || admin.Holders[0] != (Holder{Kind: HolderGroup, Name: "gentian:platform:admins"}) {
		t.Fatalf("admin holders = %v", admin.Holders)
	}
	// Setting a kernel credential is administering the cluster; seeing that one
	// is required, and who set it, is also the auditor's.
	if strings.Join(admin.Grants, ",") != "can_audit,can_configure,can_deploy_tenant,can_open_kernel_tools,can_read_credential,can_write_credential" {
		t.Fatalf("admin grants = %v", admin.Grants)
	}
	if strings.Join(by["auditor"].Grants, ",") != "can_audit,can_read_credential" {
		t.Fatalf("auditor grants = %v", by["auditor"].Grants)
	}
}

// "Nobody holds break_glass" is the single most useful thing this screen can
// say, so an unheld role is a row with no holders rather than a missing row.
func TestARoleNobodyHoldsIsStillARow(t *testing.T) {
	v, err := viewOf(context.Background(), reader(nil), "cluster:c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Bindings) == 0 {
		t.Fatal("no bindings at all")
	}
	if v.Unheld != len(v.Bindings) {
		t.Fatalf("unheld = %d of %d", v.Unheld, len(v.Bindings))
	}
	found := false
	for _, b := range v.Bindings {
		if b.Relation == "break_glass" {
			found = true
			if len(b.Holders) != 0 {
				t.Fatalf("break_glass holders = %v", b.Holders)
			}
			// It still says what it would carry, which is the point of
			// showing the row.
			if len(b.Grants) == 0 {
				t.Fatal("break_glass carries nothing, so the row says nothing")
			}
		}
	}
	if !found {
		t.Fatal("break_glass is not a row")
	}
}

// A role is what somebody can hold. An edge to another object -- which cluster
// operates a tenant -- is not a binding, and showing it as one would read as a
// permission somebody was given. Derived from the model's own metadata: no
// list of names can be right, since `tenant` is an edge on an app and
// `user` a role on a tenant and a kind of thing besides.
func TestAnEdgeToAnotherObjectIsNotABinding(t *testing.T) {
	g, err := parseModel(modelV1)
	if err != nil {
		t.Fatal(err)
	}
	roles := strings.Join(g.roles("tenant"), ",")
	if roles != "admin,auditor,perimeter_approver,user" {
		t.Fatalf("tenant roles = %q: operated_by and cluster are edges, user is a role", roles)
	}
	// And a tenant user's permissions are the user's, not the admin's.
	if strings.Join(g.grantsOf("tenant", "user"), ",") != "can_enter,can_view" {
		t.Fatalf("user grants = %v", g.grantsOf("tenant", "user"))
	}
	// On an app the roles are the two every app has; its tenant, its base,
	// its add-ons and the roles it declares are edges.
	if roles := strings.Join(g.roles("app"), ","); roles != "admin,user" {
		t.Fatalf("app roles = %q", roles)
	}
	// admin is the highest role: it carries what user carries.
	if got := strings.Join(g.grantsOf("app", "admin"), ","); got != "can_administer,can_launch,can_use" {
		t.Fatalf("an app's admin carries %s", got)
	}
}

// Who holds a role is said as a person reading the screen knows them.
func TestHoldersAreNamedAsPeopleKnowThem(t *testing.T) {
	for user, want := range map[string]Holder{
		"position:demo/sales-manager#holder":  {Kind: HolderPosition, Name: "sales-manager"},
		"group:gentian/platform/admin#member": {Kind: HolderGroup, Name: "gentian:platform:admin"},
		"user:7d0f":                           {Kind: HolderPerson, Name: "7d0f"},
		"user:kernel/7d0f":                    {Kind: HolderPerson, Name: "kernel/7d0f"},
		"cluster:c1#operator":                 {Kind: HolderPerson, Name: "cluster:c1#operator"},
	} {
		if got := holderOf(user); got != want {
			t.Errorf("holderOf(%s) = %+v, want %+v", user, got, want)
		}
	}
}

// An object id with no type is refused rather than read as one.
func TestAnObjectIdMustNameAType(t *testing.T) {
	if _, err := viewOf(context.Background(), reader(nil), "c1"); err == nil {
		t.Fatal("accepted an id with no type")
	}
}

// A read that fails is an error, not an empty view: "nobody holds anything"
// and "the graph could not be reached" must not look the same.
func TestAFailedReadIsNotAnEmptyView(t *testing.T) {
	boom := func(context.Context, Tuple) ([]Tuple, error) { return nil, errors.New("connection refused") }
	if _, err := viewOf(context.Background(), boom, "cluster:c1"); err == nil {
		t.Fatal("a failed read produced a view")
	}
}
