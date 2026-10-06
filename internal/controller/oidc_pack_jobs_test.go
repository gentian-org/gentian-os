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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestCollectOIDCAppConfigs_IncludesSidecarWithoutAppProfile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)

	parent := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue-test-app"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			// Rendered by a Composition, which is what decides that Crossplane
			// owns its sidecar's OIDC client rather than the operator.
			Package: gentianov1alpha1.PackageSpec{Composition: "app-default"},
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{
						ClientID:     "main-oidc-client",
						RedirectURIs: []string{"https://${TENANT_DOMAIN}/oidc/callback"},
					},
				},
			}},

			Extensions: []gentianov1alpha1.AppSidecarSpec{
				{
					Name: "sidecar-meet",
					ServiceRequirements: &gentianov1alpha1.ServiceRequirements{
						Identity: &gentianov1alpha1.IdentityRequirement{
							OIDC: &gentianov1alpha1.OIDCClientSpec{
								ClientID:     "sidecar-oidc-client",
								RedirectURIs: []string{"https://${TENANT_DOMAIN}/sidecar/oidc/callback"},
							},
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent).Build()
	r := &TenantReconciler{
		Client:       c,
		KernelDomain: "platform.example.com",
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "catalogue-test-app"}},
		},
	}

	configs, err := r.collectOIDCAppConfigs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("collectOIDCAppConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 OIDC configs (catalogue-test-app + sidecar), got %d", len(configs))
	}

	var sidecarCfg *oidcAppConfig
	for i := range configs {
		if configs[i].profileName == "catalogue-test-app-sidecar-meet" {
			sidecarCfg = &configs[i]
			break
		}
	}
	if sidecarCfg == nil {
		t.Fatal("expected catalogue-test-app-sidecar-meet sidecar OIDC config")
	}
	if sidecarCfg.parentProfile != "catalogue-test-app" {
		t.Errorf("parentProfile = %q, want catalogue-test-app", sidecarCfg.parentProfile)
	}

	owner, err := r.getOIDCOwnerProfile(context.Background(), *sidecarCfg)
	if err != nil {
		t.Fatalf("getOIDCOwnerProfile sidecar: %v", err)
	}
	if owner.Name != "catalogue-test-app" {
		t.Errorf("owner profile = %q, want catalogue-test-app", owner.Name)
	}
	if !crossplaneOwnsOIDCClient(owner, *sidecarCfg) {
		t.Error("expected Crossplane to own sidecar OIDC when parent has compositionRef")
	}
}
