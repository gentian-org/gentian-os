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

func TestCollectOIDCIngressSubdomainsByTenant(t *testing.T) {
	xwiki := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "xwiki"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Ingress: &gentianov1alpha1.IngressSpec{SubDomain: "wiki"},
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{
					OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "opendesk-xwiki"},
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
					OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "opendesk-synapse"},
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
