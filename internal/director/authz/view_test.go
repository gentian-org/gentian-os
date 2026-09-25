/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

// The view answers both halves. Tuples alone say a group holds "admin"; what
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
	// Back in Keycloak's spelling: the ids differ only in the separator, and
	// the person reading this screen knows the Keycloak name.
	if len(admin.Groups) != 1 || admin.Groups[0] != "gentian:platform:admins" {
		t.Fatalf("admin groups = %v", admin.Groups)
	}
	if strings.Join(admin.Grants, ",") != "can_audit,can_configure,can_deploy_tenant" {
		t.Fatalf("admin grants = %v", admin.Grants)
	}
	if strings.Join(by["auditor"].Grants, ",") != "can_audit" {
		t.Fatalf("auditor grants = %v", by["auditor"].Grants)
	}
}

// "Nobody holds break_glass" is the single most useful thing this screen can
// say, so an unheld role is a row with no groups rather than a missing row.
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
			if len(b.Groups) != 0 {
				t.Fatalf("break_glass groups = %v", b.Groups)
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
// permission somebody was given. Derived from the model's own metadata: an
// earlier draft listed the names by hand and got `member` wrong, which is the
// membership edge on type group and a real role on type tenant.
func TestAnEdgeToAnotherObjectIsNotABinding(t *testing.T) {
	g, err := parseModel(modelV1)
	if err != nil {
		t.Fatal(err)
	}
	roles := strings.Join(g.roles("tenant"), ",")
	if roles != "admin,member,perimeter_approver" {
		t.Fatalf("tenant roles = %q: operated_by and cluster are edges, member is a role", roles)
	}
	if strings.Join(g.roles("catalogue_entry"), ",") != "" {
		t.Fatalf("catalogue_entry has roles: %v", g.roles("catalogue_entry"))
	}
	// And a tenant member's permissions are the member's, not the admin's.
	if strings.Join(g.grantsOf("tenant", "member"), ",") != "can_enter,can_view" {
		t.Fatalf("member grants = %v", g.grantsOf("tenant", "member"))
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
