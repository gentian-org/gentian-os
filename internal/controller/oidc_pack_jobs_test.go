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

	parent := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue-test-app"},
		Spec: gentianov1alpha1.AppProfileSpec{
			CompositionRef: "app-default",
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{
						ClientID:     "main-oidc-client",
						RedirectURIs: []string{"https://${TENANT_DOMAIN}/oidc/callback"},
					},
				},
			},
			Sidecars: []gentianov1alpha1.AppSidecarSpec{
				{
					Name: "sidecar-meet",
					KernelRequirements: &gentianov1alpha1.KernelRequirements{
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
		KernelDomain: "desk.example.com",
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Domain: "demo.desk.example.com",
			Apps:   []gentianov1alpha1.TenantApp{{Profile: "catalogue-test-app"}},
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
