/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller"
)

// The App Store app's profile, as the chart ships it, is one the API server
// admits: every field is one the definition has (unknown fields refused) and
// every rule on the kind holds. And it comes back saying what was sent --
// where it is placed, where the store's address and its own host land.
func TestTheAPIServerAdmitsTheShippedAppStoreProfile(t *testing.T) {
	ctx := context.Background()
	profile := controller.ShippedAppStoreProfileForTest(t)
	profile.Name = "app-store-admission-probe"
	profile.Labels = nil
	if err := testClient.Create(ctx, profile, client.FieldValidation("Strict")); err != nil {
		t.Fatalf("the API server refused the shipped profile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	stored := &gentianov1alpha1.ComponentProfile{}
	waitFor(t, envtestWaitTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: profile.Name}, stored) == nil
	})
	m := stored.Spec.Package.ValueMapping.Platform
	if !stored.Spec.DefaultWhereStoreOffered || m.StoreURLKey != "store.url" || m.HostKey != "host" {
		t.Fatalf("stored: whereStoreOffered=%v storeUrlKey=%q hostKey=%q", stored.Spec.DefaultWhereStoreOffered, m.StoreURLKey, m.HostKey)
	}

	// Being told where the store is opens a way out of the cluster, so only
	// a platform-trust profile may ask.
	other := controller.ShippedAppStoreProfileForTest(t)
	other.Name = "app-store-tier-probe"
	other.Labels = nil
	other.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	for i := range other.Spec.Expose {
		other.Spec.Expose[i].ForwardToken = false
	}
	err := testClient.Create(ctx, other)
	if err == nil {
		_ = testClient.Delete(ctx, other)
		t.Fatal("a certified profile naming storeUrlKey was admitted")
	}
	if !strings.Contains(err.Error(), "storeUrlKey requires trustTier platform") {
		t.Fatalf("refused, but not for the store key: %v", err)
	}

	// And only an app is placed on tenants.
	service := controller.ShippedAppStoreProfileForTest(t)
	service.Name = "app-store-class-probe"
	service.Labels = nil
	service.Spec.Classes = []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassService}
	for i := range service.Spec.Expose {
		if service.Spec.Expose[i].Tile != nil {
			service.Spec.Expose[i].Tile.Object = gentianov1alpha1.TileObjectCluster
		}
	}
	err = testClient.Create(ctx, service)
	if err == nil {
		_ = testClient.Delete(ctx, service)
		t.Fatal("a service declaring defaultWhereStoreOffered was admitted")
	}
	if !strings.Contains(err.Error(), "defaultWhereStoreOffered is for class app") {
		t.Fatalf("refused, but not for its class: %v", err)
	}
}
