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

// The desktop's profile, as the chart ships it, is one the API server admits
// with its optional model gateway requirement and the three places its chart
// takes what the platform tells it about the gateway. A profile that is
// placed on tenants and declares the gateway below platform trust is refused
// at the door: it would be given a key on every tenant with nobody asking.
func TestTheAPIServerAdmitsTheDesktopsModelGatewayRequirement(t *testing.T) {
	ctx := context.Background()
	profile := controller.ShippedDesktopProfileForTest(t)
	profile.Name = "desktop-admission-probe"
	profile.Labels = nil
	if err := testClient.Create(ctx, profile, client.FieldValidation("Strict")); err != nil {
		t.Fatalf("the API server refused the shipped profile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	stored := &gentianov1alpha1.ComponentProfile{}
	waitFor(t, envtestWaitTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: profile.Name}, stored) == nil
	})
	llm := stored.Spec.Requires.Services.LLM
	m := stored.Spec.Package.ValueMapping.LLM
	if llm == nil || !llm.Optional || m == nil || m.AvailableKey != "llm.available" || m.SecretNameKey != "llm.apiKeySecretName" {
		t.Fatalf("stored: llm=%+v mapping=%+v", llm, m)
	}

	other := controller.ShippedDesktopProfileForTest(t)
	other.Name = "desktop-tier-probe"
	other.Labels = nil
	other.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	for i := range other.Spec.Expose {
		other.Spec.Expose[i].ForwardToken = false
	}
	err := testClient.Create(ctx, other)
	if err == nil {
		_ = testClient.Delete(ctx, other)
		t.Fatal("a certified profile placed on tenants and declaring the model gateway was admitted")
	}
	if !strings.Contains(err.Error(), "declares requires.services.llm only at trustTier platform") {
		t.Fatalf("refused, but not for the model gateway: %v", err)
	}
}
