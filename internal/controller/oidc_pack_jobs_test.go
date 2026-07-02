/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

	element := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "od-element"},
		Spec: gentianov1alpha1.AppProfileSpec{
			CompositionRef: "app-od-element",
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{
						ClientID: "main-oidc-client",
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
			Apps:   []gentianov1alpha1.TenantApp{{Profile: "od-element"}},
		},
	}

	configs, err := r.collectOIDCAppConfigs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("collectOIDCAppConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 OIDC configs (od-element + od-element-jitsi sidecar), got %d", len(configs))
	}

	var sidecarCfg *oidcAppConfig
	for i := range configs {
		if configs[i].profileName == "od-element-jitsi" {
			sidecarCfg = &configs[i]
			break
		}
	}
	if sidecarCfg == nil {
		t.Fatal("expected od-element-jitsi sidecar OIDC config")
	}
	if sidecarCfg.parentProfile != "od-element" {
		t.Errorf("parentProfile = %q, want od-element", sidecarCfg.parentProfile)
	}

	owner, err := r.getOIDCOwnerProfile(context.Background(), *sidecarCfg)
	if err != nil {
		t.Fatalf("getOIDCOwnerProfile sidecar: %v", err)
	}
	if owner.Name != "od-element" {
		t.Errorf("owner profile = %q, want od-element", owner.Name)
	}
	if !crossplaneOwnsOIDCClient(owner, *sidecarCfg) {
		t.Error("expected Crossplane to own sidecar OIDC when parent has compositionRef")
	}
}
