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

func TestCollectOIDCIngressSubdomainsByTenant(t *testing.T) {
	xwiki := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "xwiki"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Ingress: &gentianov1alpha1.IngressSpec{SubDomain: "wiki"},
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "wiki-oidc-client"},
				},
			},
		},
	}
	element := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "element"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Ingress: &gentianov1alpha1.IngressSpec{SubDomain: "chat"},
			AdditionalIngresses: []gentianov1alpha1.IngressSpec{
				{SubDomain: "matrix"},
			},
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "chat-oidc-client"},
				},
			},
		},
	}
	tenant := gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "xwiki"},
				{Profile: "element"},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(xwiki, element).Build()

	got, err := collectOIDCIngressSubdomainsByTenant(context.Background(), c, []gentianov1alpha1.Tenant{tenant})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	subs := got["demo"]
	if len(subs) != 3 {
		t.Fatalf("expected 3 subdomains, got %v", subs)
	}
	want := map[string]struct{}{
		"wiki":   {},
		"chat":   {},
		"matrix": {},
	}
	for _, sub := range subs {
		if _, ok := want[sub]; !ok {
			t.Fatalf("unexpected subdomain %q in %v", sub, subs)
		}
	}
}

func TestOIDCRedirectURISubdomain(t *testing.T) {
	if got := oidcRedirectURISubdomain("https://matrix.${TENANT_DOMAIN}/_synapse/client/oidc/callback"); got != "matrix" {
		t.Fatalf("expected matrix, got %q", got)
	}
	if got := oidcRedirectURISubdomain("https://chat.${TENANT_DOMAIN}/"); got != "chat" {
		t.Fatalf("expected chat, got %q", got)
	}
	if got := oidcRedirectURISubdomain("not-a-url"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestCollectOIDCIngressSubdomainsFromRedirectURI(t *testing.T) {
	element := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "element"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Ingress: &gentianov1alpha1.IngressSpec{SubDomain: "chat"},
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{
						ClientID: "chat-oidc-client",
						RedirectURIs: []string{
							"https://matrix.${TENANT_DOMAIN}/_synapse/client/oidc/callback",
						},
					},
				},
			},
		},
	}
	subs := oidcIngressSubdomainsFromProfile(element)
	want := map[string]struct{}{"chat": {}, "matrix": {}}
	if len(subs) != len(want) {
		t.Fatalf("expected %d subdomains, got %v", len(want), subs)
	}
	for _, sub := range subs {
		if _, ok := want[sub]; !ok {
			t.Fatalf("unexpected subdomain %q", sub)
		}
	}
}
