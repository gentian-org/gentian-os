/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// A component the platform places gets the secrets its profile has the
// platform generate, as an installed app does from the tenant's loop: its
// chart's sensitive values read them, and without them it waits at
// ValuesPending for good. Seeded once; a later pass keeps the value.
func TestAPlacedComponentGetsTheSecretsItsProfileHasGenerated(t *testing.T) {
	profile := materialised(t, wikiBundle)
	profile.Annotations = nil
	profile.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
	profile.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{
		Generated: []gentianov1alpha1.AppSecret{{Name: "key_seed", ValuePath: "secrets.keySeed"}},
	}
	if !operatorPlaced(profile) {
		t.Fatal("the fixture is meant to be a placed component")
	}
	h := startDigestHarness(t, profile, "")
	vault := memVault{}
	h.r.Seeder = secrets.NewSeeder(vault, nil)

	h.reconcile()
	path := secrets.InternalPath("acme", h.comp.Name, "key_seed")
	first := vault[path]["value"]
	if first == "" {
		t.Fatalf("no generated secret at %s; vault = %v", path, vault)
	}
	h.reconcile()
	if vault[path]["value"] != first {
		t.Fatal("a later pass replaced the generated secret")
	}
}

// An installed app's generated secrets are the tenant's loop's to make: the
// Component reconciler leaves them alone.
func TestAnInstalledComponentsGeneratedSecretsAreNotSeededHere(t *testing.T) {
	profile := materialised(t, wikiBundle)
	profile.Annotations = nil
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{
		Generated: []gentianov1alpha1.AppSecret{{Name: "key_seed", ValuePath: "secrets.keySeed"}},
	}
	if operatorPlaced(profile) {
		t.Fatal("the fixture is meant to be an installed app")
	}
	h := startDigestHarness(t, profile, "")
	vault := memVault{}
	h.r.Seeder = secrets.NewSeeder(vault, nil)
	h.reconcile()
	if v, ok := vault[secrets.InternalPath("acme", h.comp.Name, "key_seed")]; ok {
		t.Fatalf("the Component reconciler seeded an installed app's secret: %v", v)
	}
}
