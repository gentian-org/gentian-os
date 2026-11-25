/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// tupleStore is a rights store in memory, read as OpenFGA reads: by the
// fields named, an object named by its type alone matching every object of
// that type.
type tupleStore []authz.Tuple

func (s tupleStore) Read(_ context.Context, f authz.Tuple) ([]authz.Tuple, error) {
	var out []authz.Tuple
	for _, t := range s {
		if f.User != "" && t.User != f.User || f.Relation != "" && t.Relation != f.Relation {
			continue
		}
		if o := f.Object; o != "" && t.Object != o && (!strings.HasSuffix(o, ":") || !strings.HasPrefix(t.Object, o)) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (s tupleStore) Write(context.Context, []authz.Tuple, []authz.Tuple) error { return nil }

type handedRoles map[string][]authz.RoleHolding

func (h handedRoles) SetRoles(_ context.Context, tenant, component string, holdings []authz.RoleHolding) error {
	if component == "late" {
		return arbiter.ErrNotHanded
	}
	h[tenant+"/"+component] = holdings
	return nil
}

// A store whose profile asks for its people's roles is handed them after the
// pass; one that does not ask is handed nothing, and one the arbiter does not
// list yet is left for a later pass.
func TestTheStoresThatAskAreHandedTheirPeoplesRoles(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	handing := func(name string) *gentianov1alpha1.ComponentProfile {
		p := authorizationProfile(name, ledgerModel)
		p.Spec.Requires.Services.Authorization.Roles = gentianov1alpha1.AuthorizationRolesHandOver
		return p
	}
	ledger, plain, late := rightsComponent("acme", "ledger"), rightsComponent("acme", "plain"), rightsComponent("acme", "late")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rightsTenant("acme"), ledger, plain, late,
		handing("ledger"), authorizationProfile("plain", ledgerModel), handing("late")).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	for _, comp := range []*gentianov1alpha1.Component{ledger, plain, late} {
		if err := r.ensureAuthorization(ctx, comp, defaultTenantName(comp), authorizationProfile(comp.Name, ledgerModel)); err != nil {
			t.Fatal(err)
		}
	}
	store := tupleStore{
		{User: "tenant:acme", Relation: "tenant", Object: "app:acme/ledger"},
		{User: "role:acme/ledger/sales", Relation: "declared", Object: "app:acme/ledger"},
		{User: "app:acme/ledger", Relation: "app", Object: "role:acme/ledger/sales"},
		{User: "user:anna", Relation: "holder", Object: "role:acme/ledger/sales"},
	}
	got := handedRoles{}
	if err := handOverRoles(ctx, c, store, got); err != nil {
		t.Fatal(err)
	}
	want := handedRoles{"acme/ledger": {{Subject: "anna", Roles: []string{"sales", "user"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("handed %+v, want %+v", got, want)
	}
}
