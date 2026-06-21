// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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

	element := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "element"},
		Spec: gentianov1alpha1.AppProfileSpec{
			CompositionRef: "app-element",
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{
						ClientID: "opendesk-synapse",
					},
				},
			},
			Sidecars: []gentianov1alpha1.AppSidecarSpec{
				{
					Name: "jitsi",
					KernelRequirements: &gentianov1alpha1.KernelRequirements{
						Identity: &gentianov1alpha1.IdentityRequirement{
							OIDC: &gentianov1alpha1.OIDCClientSpec{
								ClientID: "meet-sidecar-client",
							},
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(element).Build()
	r := &TenantReconciler{
		Client:       c,
		KernelDomain: "desk.example.com",
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Domain: "demo.desk.example.com",
			Apps:   []gentianov1alpha1.TenantApp{{Profile: "element"}},
		},
	}

	configs, err := r.collectOIDCAppConfigs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("collectOIDCAppConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 OIDC configs (element + element-jitsi sidecar), got %d", len(configs))
	}

	var sidecarCfg *oidcAppConfig
	for i := range configs {
		if configs[i].profileName == "element-jitsi" {
			sidecarCfg = &configs[i]
			break
		}
	}
	if sidecarCfg == nil {
		t.Fatal("expected element-jitsi sidecar OIDC config")
	}
	if sidecarCfg.parentProfile != "element" {
		t.Errorf("parentProfile = %q, want element", sidecarCfg.parentProfile)
	}

	owner, err := r.getOIDCOwnerProfile(context.Background(), *sidecarCfg)
	if err != nil {
		t.Fatalf("getOIDCOwnerProfile sidecar: %v", err)
	}
	if owner.Name != "element" {
		t.Errorf("owner profile = %q, want element", owner.Name)
	}
	if !crossplaneOwnsOIDCClient(owner, *sidecarCfg) {
		t.Error("expected Crossplane to own sidecar OIDC when parent has compositionRef")
	}
}
