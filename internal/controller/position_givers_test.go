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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

func positionGiversList(t *testing.T, c client.Client) (string, []registrar.PositionGiver) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: "registrar-position-givers", Namespace: layout.Namespace(layout.Control)}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("the registrar's list: %v", err)
	}
	raw := cm.Data["keys.json"]
	givers, err := registrar.ParsePositionGivers([]byte(raw))
	if err != nil {
		t.Fatalf("the registrar refuses the operator's list: %v\n%s", err, raw)
	}
	return raw, givers
}

// A component that holds a key is listed for the positions each tenant
// that names it names, and for nothing else: a component no tenant names is
// not listed, one that holds no key is not either, and a position that
// administers -- an administrator's, or one holding admin itself or through
// an inclusion -- is listed apart as administering, never as given. The list carries the key's
// hash, never the key, and is written empty when no tenant names anybody.
func TestRegistrarListsAPositionGiverForWhatTheTenantsName(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	checkout, idle, plain := rightsComponent("shop", "checkout"), rightsComponent("shop", "idle"), rightsComponent("shop", "plain")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"checkout": rightsProfile("checkout", true), "idle": rightsProfile("idle", true), "plain": rightsProfile("plain", false),
	}
	customers := rightsTenant("customers")
	// deputy administers an app, and chief through deputy.
	customers.Spec.Positions = []gentianov1alpha1.TenantPosition{
		{Name: "customer"},
		{Name: "deputy", Roles: []gentianov1alpha1.TenantPositionRole{{App: "shop", Role: "admin"}}},
		{Name: "chief", Includes: []string{"deputy"}},
	}
	customers.Spec.PositionGivers = []gentianov1alpha1.TenantPositionGiver{
		{Tenant: "shop", Component: "checkout", Positions: []string{"customer", "tenant-administrator", "chief"}},
		{Tenant: "shop", Component: "plain", Positions: []string{"customer"}},
	}
	students := rightsTenant("students")
	students.Spec.PositionGivers = []gentianov1alpha1.TenantPositionGiver{
		{Tenant: "shop", Component: "checkout", Positions: []string{"user", "student"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		rightsTenant("shop"), customers, students, checkout, idle, plain, profiles["checkout"], profiles["idle"], profiles["plain"],
	).Build()
	components := &ComponentReconciler{Client: c, Scheme: scheme}
	for _, comp := range []*gentianov1alpha1.Component{checkout, idle, plain} {
		if err := components.ensureRightsCheck(ctx, comp, defaultTenantName(comp), profiles[comp.Name]); err != nil {
			t.Fatal(err)
		}
	}

	r := &GatewayPlatformReconciler{Client: c}
	if err := r.ensureRegistrarPositionGivers(ctx); err != nil {
		t.Fatal(err)
	}
	raw, givers := positionGiversList(t, c)
	secret := rightsSecret(t, c, checkout)
	sum := sha256.Sum256(secret.Data[rightsCheckKeyKey])
	if len(givers) != 1 || givers[0].Tenant != "shop" || givers[0].Component != "checkout" || givers[0].KeyHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("the list is %s", raw)
	}
	if got := fmt.Sprint(givers[0].Gives); got != "[{customers [customer] [chief tenant-administrator]} {students [student user] []}]" {
		t.Fatalf("the checkout gives %s", got)
	}
	if strings.Contains(raw, string(secret.Data[rightsCheckKeyKey])) {
		t.Fatal("the list carries a key itself")
	}

	// The tenants stop naming it: the list is written empty, not left.
	for _, name := range []string{"customers", "students"} {
		current := &gentianov1alpha1.Tenant{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, current); err != nil {
			t.Fatal(err)
		}
		current.Spec.PositionGivers = nil
		if err := c.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ensureRegistrarPositionGivers(ctx); err != nil {
		t.Fatal(err)
	}
	if after, givers := positionGiversList(t, c); len(givers) != 0 {
		t.Fatalf("after the tenants stopped naming it: %s", after)
	}
}

// A component a tenant names as one that gives its positions is told where
// the registrar is, in its key Secret; one no tenant names is not, and is
// no longer told once the tenant stops naming it.
func TestAPositionGiverIsToldWhereTheRegistrarIs(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	shop := rightsComponent("team", "shop")
	plain := rightsComponent("team", "plain")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"shop": partnerCommissionProfile("shop"), "plain": partnerCommissionProfile("plain"),
	}
	customers := commissionTenant("customers")
	customers.Spec.PositionGivers = []gentianov1alpha1.TenantPositionGiver{{Tenant: "team", Component: "shop", Positions: []string{"partner"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		rightsTenant("team"), shop, plain, profiles["shop"], profiles["plain"], customers,
	).Build()
	components := &ComponentReconciler{Client: c, Scheme: scheme, RegistrarURL: "http://registrar.test:9445"}
	ensure := func() {
		t.Helper()
		for _, comp := range []*gentianov1alpha1.Component{shop, plain} {
			if err := components.ensureRightsCheck(ctx, comp, defaultTenantName(comp), profiles[comp.Name]); err != nil {
				t.Fatal(err)
			}
		}
	}
	ensure()
	if got := string(rightsSecret(t, c, shop).Data[registrarURLKey]); got != "http://registrar.test:9445" {
		t.Fatalf("the shop's Secret carries the registrar at %q", got)
	}
	if _, has := rightsSecret(t, c, plain).Data[registrarURLKey]; has {
		t.Fatal("a component no tenant names is told where the registrar is")
	}

	named := &gentianov1alpha1.Tenant{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(customers), named); err != nil {
		t.Fatal(err)
	}
	named.Spec.PositionGivers = nil
	if err := c.Update(ctx, named); err != nil {
		t.Fatal(err)
	}
	ensure()
	if _, has := rightsSecret(t, c, shop).Data[registrarURLKey]; has {
		t.Fatal("a component no longer named is still told where the registrar is")
	}
}
