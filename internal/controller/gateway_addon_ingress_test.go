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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An Odoo-shaped tenant: a base that owns the ERP host, and an addon that wants
// a hostname of its own for the site it publishes.
func addonIngressFixture() (*gentianov1alpha1.Tenant, []runtime.Object) {
	base := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "odoo-base-ce"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Expose: []gentianov1alpha1.ExposureSpec{{
				Name: "web", Surface: gentianov1alpha1.SurfaceGateway,
				AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "erp",
				Backend: gentianov1alpha1.BackendRef{Service: "odoo", Port: 8069},
			}},
		},
	}
	addon := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "odoo-website-ce"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			// An addon's extra host. It is the FIRST gateway entry of this
			// profile, and the base's is the first of the base's -- an addon
			// has no primary host of its own, which is what the reconciler
			// reads by taking gateways[1:] for additional hosts.
			Expose: []gentianov1alpha1.ExposureSpec{{
				Name: "web", Surface: gentianov1alpha1.SurfaceGateway,
				AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "erp",
				Backend: gentianov1alpha1.BackendRef{Service: "odoo", Port: 8069},
			}, {
				Name: "site", Surface: gentianov1alpha1.SurfaceGateway,
				AuthMode: gentianov1alpha1.AuthModeNone, SubDomain: "www",
				Backend: gentianov1alpha1.BackendRef{Service: "odoo", Port: 8069},
			}},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant",
			Apps: []gentianov1alpha1.TenantApp{{
				Profile: "odoo-base-ce",
				Addons:  []string{"odoo-website-ce"},
			}},
		},
	}
	return tenant, []runtime.Object{base, addon}
}

func TestCollectTenantIngressIntentsIncludesAddonHosts(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	tenant, objs := addonIngressFixture()
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	intents, err := collectTenantIngressIntents(t.Context(), c, tenant)
	if err != nil {
		t.Fatalf("collect intents: %v", err)
	}

	got := make(map[string]string, len(intents))
	for _, in := range intents {
		got[in.appProfile] = ingressHost(in.appProfile, in.ingress, "demo.desk.gentian.org")
	}

	// The base still owns its own host.
	if host := got["odoo-base-ce"]; host != "erp.demo.desk.gentian.org" {
		t.Errorf("base host = %q, want erp.demo.desk.gentian.org", host)
	}

	// The addon's host is present, and named for the addon rather than the base
	// so its route, certificate and DNS record go away with the addon.
	host, ok := got["odoo-website-ce-extra0"]
	if !ok {
		t.Fatalf("addon ingress produced no intent; got %v", got)
	}
	if host != "www.demo.desk.gentian.org" {
		t.Errorf("addon host = %q, want www.demo.desk.gentian.org", host)
	}

	if len(intents) != 2 {
		t.Errorf("expected exactly the base and addon intents, got %d: %v", len(intents), got)
	}
}

func TestCollectTenantIngressIntentsIgnoresAddonPrimaryIngress(t *testing.T) {
	t.Parallel()

	// An addon is reached inside its base and must never claim a primary host.
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	tenant, objs := addonIngressFixture()
	addon := objs[1].(*gentianov1alpha1.ComponentProfile)
	*addon.GatewayExposures()[0] = gentianov1alpha1.ExposureSpec{
		Name: "web", Surface: gentianov1alpha1.SurfaceGateway,
		AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "hijack",
		Backend: gentianov1alpha1.BackendRef{Service: "odoo", Port: 8069},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	intents, err := collectTenantIngressIntents(t.Context(), c, tenant)
	if err != nil {
		t.Fatalf("collect intents: %v", err)
	}
	for _, in := range intents {
		if in.ingress.SubDomain == "hijack" {
			t.Fatalf("addon Spec.Ingress was honoured as %q; addons get no primary host", in.appProfile)
		}
	}
}
