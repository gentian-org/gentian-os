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
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// appClaimOf is the App claim of a component's name in its namespace, with
// an owner reference of the given kind and name, the controller one or not.
func appClaimOf(comp *gentianov1alpha1.Component, kind, name string, controller bool) *unstructured.Unstructured {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	claim.SetName(comp.Name)
	claim.SetNamespace(comp.Namespace)
	if kind != "" {
		uid := types.UID("uid-owner")
		// The component's own claim names the component by its UID, as an
		// owner reference does.
		if kind == "Component" && name == comp.Name {
			if comp.UID == "" {
				comp.UID = types.UID("uid-" + comp.Name)
			}
			uid = comp.UID
		}
		claim.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "gentianos.io/v1alpha1", Kind: kind, Name: name, UID: uid,
			Controller: &controller,
		}})
	}
	return claim
}

func getComponent(t *testing.T, c client.Client, comp *gentianov1alpha1.Component) *gentianov1alpha1.Component {
	t.Helper()
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(comp), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A contract selects an installed app by the app label every installed
// chart's pods are rendered with, whether the operator or the Composition
// installed it (its claim still there), and a placed one by its release.
// Selecting an installed app by a release label would let the contract
// through to no pod at all: not every chart sets it.
func TestAContractSelectsAnInstalledAppByItsAppLabel(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)

	desktop := chartProfileFixture("oci://registry.example/gentian")
	desktop.Name = "desktop"
	desktop.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
	desktop.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	desktop.Spec.Provides = []gentianov1alpha1.ContractRef{{
		Name: "desktop-api", Protocol: "http",
		Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "desktop-gentian-portal-api", Port: 8000},
	}}
	store := mappingProfile(map[string]gentianov1alpha1.IntegrationValueMapping{
		"desktop-api": {EndpointKey: "api.env.DESKTOP_API_URL"},
	})
	store.Name = "store"
	store.Spec.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "desktop-api", Provider: "desktop", Capabilities: []string{"tiles"}}}
	if operatorPlaced(store) || !operatorPlaced(desktop) {
		t.Fatal("the fixture is meant to be an installed store and a placed desktop")
	}

	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "uid-acme"}}
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{placedEntry("desktop"), {Profile: "store"}}
	tenant.Spec.Grants = []gentianov1alpha1.TenantAppGrant{{AppGrantSpec: gentianov1alpha1.AppGrantSpec{
		App: "store", Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "desktop-api", Granted: []string{"tiles"}}},
	}}}
	ns := tenantNamespaceName(tenant)
	storeComp := componentFor("store", ns)
	claim := appClaimOf(storeComp, "Component", "store", true)
	vault := memVault{secrets.ContractPath(tenant.Name, "desktop-api"): {"password": "shared", "endpoint": "", "user": ""}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), desktop, store, storeComp, claim).
		WithStatusSubresource(&gentianov1alpha1.IntegrationBinding{}).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme, Seeder: secrets.NewSeeder(vault, nil)}
	if err := tr.ensureAppGrants(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	bindings, err := tr.collectDesiredIntegrationBindings(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want the store's one", len(bindings))
	}
	if err := c.Create(ctx, bindings[0]); err != nil {
		t.Fatal(err)
	}
	if err := tr.ensureNetworkPolicies(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	out := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "contract-" + bindings[0].Name, Namespace: ns}, out); err != nil {
		t.Fatalf("no way out for the consumer: %v", err)
	}
	if want := map[string]string{appLabel: "store"}; !equalLabels(out.Spec.PodSelector.MatchLabels, want) {
		t.Fatalf("consumer selected by %v, want %v", out.Spec.PodSelector.MatchLabels, want)
	}
	// The desktop is placed and selected by its release.
	if len(out.Spec.Egress) != 1 || len(out.Spec.Egress[0].To) != 1 ||
		!equalLabels(out.Spec.Egress[0].To[0].PodSelector.MatchLabels, map[string]string{componentInstanceLabel: ns + "-desktop"}) {
		t.Fatalf("consumer egress = %+v", out.Spec.Egress)
	}
}

// The handoff does not take an App claim of the component's name that is not
// the component's own -- nobody controls it, something else does, or an
// earlier component of the same name did: what it composed is somebody
// else's. The component waits and says why. Its own claim that composed
// nothing yet is simply deleted: there is nothing to take over.
func TestTheHandoffWaitsForAClaimThatIsNotTheComponentsOwn(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	for name, claimOf := range map[string]func(*gentianov1alpha1.Component) *unstructured.Unstructured{
		"nobody's": func(comp *gentianov1alpha1.Component) *unstructured.Unstructured {
			return appClaimOf(comp, "", "", false)
		},
		"the tenant's": func(comp *gentianov1alpha1.Component) *unstructured.Unstructured {
			return appClaimOf(comp, "Tenant", "acme", true)
		},
		"an earlier one": func(*gentianov1alpha1.Component) *unstructured.Unstructured {
			return appClaimOf(&gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-acme", UID: "uid-before"}}, "Component", "wiki", true)
		},
		"its own": func(comp *gentianov1alpha1.Component) *unstructured.Unstructured {
			return appClaimOf(comp, "Component", "wiki", true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			comp := componentFor("wiki", "tenant-acme")
			comp.UID = "uid-now"
			claim := claimOf(comp)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, claim).Build()
			r := &ComponentReconciler{Client: c, Scheme: scheme}
			waiting, err := r.handOff(ctx, getComponent(t, c, comp))
			if err != nil {
				t.Fatal(err)
			}
			still := &unstructured.Unstructured{}
			still.SetGroupVersionKind(appClaimGVK)
			gone := apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(claim), still))
			if name == "its own" {
				if !gone || !strings.Contains(waiting, "composed nothing") {
					t.Fatalf("its own empty claim: gone %v, waiting %q", gone, waiting)
				}
				return
			}
			if gone {
				t.Fatal("a claim that is not the component's was deleted")
			}
			if !strings.Contains(waiting, "not this component's") {
				t.Fatalf("waiting = %q, want it to say the claim is not the component's", waiting)
			}
		})
	}
}
