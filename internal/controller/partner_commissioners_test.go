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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func partnerCommissionProfile(name string) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{Partners: &gentianov1alpha1.PartnersRequirement{Commission: true}},
	}
	return profile
}

func commissionTenant(name string, commissioners ...gentianov1alpha1.TenantComponentRef) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	t.Spec.PartnerCommissioners = commissioners
	return t
}

func directorsList(t *testing.T, c client.Client) (string, []api.CommissionKey) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: "director-commission-keys", Namespace: layout.Namespace(layout.Control)}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("the director's list: %v", err)
	}
	raw := cm.Data["keys.json"]
	keys, err := api.ParseCommissionKeys([]byte(raw))
	if err != nil {
		t.Fatalf("the director refuses the operator's list: %v\n%s", err, raw)
	}
	return raw, keys
}

// A component is given a key, the director's address and nothing that
// admits it anywhere, until a tenant names it: then the director's list
// holds its hash with that tenant. A tenant that names a component whose
// profile does not declare the requirement grants nothing, and neither does
// a profile that declares it for a component no tenant names.
func TestDirectorListHoldsAComponentOnlyForTheTenantsThatNameIt(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	shop := rightsComponent("team", "shop")
	idle := rightsComponent("team", "idle")
	plain := rightsComponent("team", "plain")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"shop": partnerCommissionProfile("shop"), "idle": partnerCommissionProfile("idle"), "plain": rightsProfile("plain", true),
	}
	customers := commissionTenant("customers",
		gentianov1alpha1.TenantComponentRef{Tenant: "team", Profile: "shop"},
		gentianov1alpha1.TenantComponentRef{Tenant: "team", Profile: "plain"})
	other := commissionTenant("other")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		shop, idle, plain, profiles["shop"], profiles["idle"], profiles["plain"], customers, other,
	).Build()
	components := &ComponentReconciler{Client: c, Scheme: scheme, DirectorURL: "http://director.test:8080"}
	for _, comp := range []*gentianov1alpha1.Component{shop, idle, plain} {
		if err := components.ensureRightsCheck(ctx, comp, profiles[comp.Name]); err != nil {
			t.Fatal(err)
		}
	}
	secret := rightsSecret(t, c, shop)
	if got := string(secret.Data[componentDirectorURLKey]); got != "http://director.test:8080" {
		t.Fatalf("the shop's Secret carries the director at %q", got)
	}
	if _, has := rightsSecret(t, c, plain).Data[componentDirectorURLKey]; has {
		t.Fatal("a component that commissions nothing is told where the director is")
	}
	// Commissioning is no rights check: the bouncer hears only of plain.
	if checkers, err := rightsCheckers(ctx, c, bouncerCheckerAudit{}); err != nil || len(checkers) != 1 || checkers[0].Component != "plain" {
		t.Fatalf("the bouncer's checkers: %+v, %v", checkers, err)
	}

	r := &GatewayPlatformReconciler{Client: c}
	if err := r.ensureDirectorCommissionKeys(ctx); err != nil {
		t.Fatal(err)
	}
	raw, keys := directorsList(t, c)
	sum := sha256.Sum256(secret.Data[rightsCheckKeyKey])
	if len(keys) != 1 || keys[0].Tenant != "team" || keys[0].Component != "shop" ||
		keys[0].KeyHash != hex.EncodeToString(sum[:]) || !slices.Equal(keys[0].Tenants, []string{"customers"}) {
		t.Fatalf("the list is %s", raw)
	}
	if strings.Contains(raw, string(secret.Data[rightsCheckKeyKey])) {
		t.Fatal("the list carries a key itself")
	}

	// The tenant stops naming it: the list is written empty, not left.
	current := &gentianov1alpha1.Tenant{}
	if err := c.Get(ctx, types.NamespacedName{Name: "customers"}, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.PartnerCommissioners = nil
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureDirectorCommissionKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if after, keys := directorsList(t, c); len(keys) != 0 {
		t.Fatalf("after the tenant stopped naming it: %s", after)
	}
}

// A component that commissions organizations reaches the control namespace,
// where the director is; one that does not, does not.
func TestACommissionerReachesTheDirector(t *testing.T) {
	r := &ComponentReconciler{}
	control := layout.Namespace(layout.Control)
	if got := r.componentEgressNamespaces(nil, partnerCommissionProfile("shop"), nil); !slices.Contains(got, control) {
		t.Fatalf("a commissioner's namespaces: %v", got)
	}
	if got := r.componentEgressNamespaces(nil, rightsProfile("plain", false), nil); slices.Contains(got, control) {
		t.Fatalf("a plain component's namespaces: %v", got)
	}
}
