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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/registrar"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

func vouchingProfile(name string) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{Vouching: &gentianov1alpha1.VouchingRequirement{
			Keys: gentianov1alpha1.VouchingKeys{Service: name, Port: 8080},
		}},
	}
	return profile
}

func registrarsList(t *testing.T, c client.Client) (string, []registrar.VouchingKey) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: "registrar-vouching-keys", Namespace: layout.Namespace(layout.Control)}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("the registrar's list: %v", err)
	}
	raw := cm.Data["keys.json"]
	keys, err := registrar.ParseVouchingKeys([]byte(raw))
	if err != nil {
		t.Fatalf("the registrar refuses the operator's list: %v\n%s", err, raw)
	}
	return raw, keys
}

// The registrar's list holds the components that vouch for people and no
// other: not one that holds a key for the rights check alone. It carries the
// hash of each key and never the key, and an entry leaves it when the
// profile stops declaring vouching, though the Secret is still there.
func TestRegistrarListHoldsOnlyComponentsThatVouch(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	notary, checker, plain := rightsComponent("acme", "notary"), rightsComponent("acme", "checker"), rightsComponent("acme", "plain")
	elsewhere := rightsComponent("globex", "notary")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"notary": vouchingProfile("notary"), "checker": rightsProfile("checker", true), "plain": rightsProfile("plain", false),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		notary, checker, plain, elsewhere, profiles["notary"], profiles["checker"], profiles["plain"],
	).Build()
	components := &ComponentReconciler{Client: c, Scheme: scheme}
	for _, comp := range []*gentianov1alpha1.Component{notary, checker, plain, elsewhere} {
		if err := components.ensureRightsCheck(ctx, comp, profiles[comp.Name]); err != nil {
			t.Fatal(err)
		}
	}
	hashOf := func(comp *gentianov1alpha1.Component) (string, string) {
		key := rightsSecret(t, c, comp).Data[rightsCheckKeyKey]
		sum := sha256.Sum256(key)
		return string(key), hex.EncodeToString(sum[:])
	}
	notaryKey, notaryHash := hashOf(notary)
	elsewhereKey, elsewhereHash := hashOf(elsewhere)
	checkerKey, _ := hashOf(checker)

	// The bouncer still hears of all three that hold a key.
	if checkers, err := rightsCheckers(ctx, c); err != nil || len(checkers) != 3 {
		t.Fatalf("the bouncer's checkers: %+v, %v", checkers, err)
	}

	r := &GatewayPlatformReconciler{Client: c}
	if err := r.ensureRegistrarVouchingKeys(ctx); err != nil {
		t.Fatal(err)
	}
	raw, keys := registrarsList(t, c)
	want := []registrar.VouchingKey{
		{Tenant: "acme", Component: "notary", KeyHash: notaryHash},
		{Tenant: "globex", Component: "notary", KeyHash: elsewhereHash},
	}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("the list is %+v, want %+v", keys, want)
	}
	for _, key := range []string{notaryKey, elsewhereKey, checkerKey} {
		if strings.Contains(raw, key) {
			t.Fatal("the list carries a key itself")
		}
	}

	// Written again with nothing changed, it is the same list.
	if err := r.ensureRegistrarVouchingKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := registrarsList(t, c); again != raw {
		t.Fatalf("the list changed with nothing to change it:\n%s\n%s", raw, again)
	}

	// The profile stops declaring vouching and keeps the rights check: both
	// components that run it leave the registrar's list, and the bouncer's
	// table keeps them.
	current := &gentianov1alpha1.ComponentProfile{}
	if err := c.Get(ctx, types.NamespacedName{Name: "notary"}, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Requires.Services.Vouching = nil
	current.Spec.Requires.Services.Rights = &gentianov1alpha1.RightsRequirement{}
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureRegistrarVouchingKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if after, keys := registrarsList(t, c); len(keys) != 0 || strings.Contains(after, notaryHash) {
		t.Fatalf("after the profile stopped declaring vouching: %s", after)
	}
	if checkers, err := rightsCheckers(ctx, c); err != nil || len(checkers) != 3 {
		t.Fatalf("the bouncer's checkers afterwards: %+v, %v", checkers, err)
	}
}

// The entry names the profile a component runs, because that is what the
// realm's entry is named after and what the registrar's route names.
func TestRegistrarListNamesTheProfile(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	comp := rightsComponent("acme", "front-desk")
	comp.Spec.ProfileRef.Name = "notary"
	profile := vouchingProfile("notary")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, profile).Build()
	if err := (&ComponentReconciler{Client: c, Scheme: scheme}).ensureRightsCheck(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	keys, err := vouchingKeys(ctx, c)
	if err != nil || len(keys) != 1 || keys[0].Tenant != "acme" || keys[0].Component != "notary" {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
}

// The operator composes the realm's entry and the registrar links people to
// it, each under a name it derives itself. They have to be the same name.
func TestOperatorAndRegistrarAgreeOnTheAlias(t *testing.T) {
	for _, profile := range []string{"notary", "a", "front-desk-2"} {
		if VouchingAlias(profile) != identity.VouchingAlias(profile) {
			t.Fatalf("the operator names %q, the registrar %q", VouchingAlias(profile), identity.VouchingAlias(profile))
		}
	}
}
