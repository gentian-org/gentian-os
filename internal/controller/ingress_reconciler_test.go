// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var certManagerCertGVKTest = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "Certificate",
}

// newIngressProfile builds an AppProfile whose spec includes an IngressSpec.
func newIngressProfile(name string, ingress *gentianov1alpha1.IngressSpec) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "0.1.0",
			},
			Ingress: ingress,
		},
	}
}

// TestIngress_NoIngressApps: no app has an IngressSpec; IngressReady=True with reason NoIngressConfigured.
func TestIngress_NoIngressApps(t *testing.T) {
	t.Parallel()
	profile := newIngressProfile("ingress-profile-none", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "ingress-none"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "No Ingress Co",
			Domain:      "ingress-none.example.com",
			AdminEmail:  "admin@ingress-none.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "ingress-profile-none"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 15*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "ingress-none"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "IngressReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("expected IngressReady condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected IngressReady=True, got %v", cond.Status)
	}
	if cond.Reason != "NoIngressConfigured" {
		t.Errorf("expected reason NoIngressConfigured, got %q", cond.Reason)
	}
}

// TestIngress_CreatesIngressResource: AppProfile with IngressSpec causes Ingress creation.
func TestIngress_CreatesIngressResource(t *testing.T) {
	t.Parallel()
	profileName := "ingress-profile-basic"
	profile := newIngressProfile(profileName, &gentianov1alpha1.IngressSpec{
		ServiceName:   "my-svc",
		ServicePort:   8080,
		SubDomain:     "app",
		TLSEnabled:    true,
		ClusterIssuer: "letsencrypt-staging",
	})
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenantName := "ingress-basic"
	domain := "ingress-basic.example.com"
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Basic Ingress Co",
			Domain:      domain,
			AdminEmail:  "admin@ingress-basic.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: profileName}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	nsName := fmt.Sprintf("tenant-%s", tenantName)
	ingressName := fmt.Sprintf("ingress-%s-%s", tenantName, profileName)

	ing := &networkingv1.Ingress{}
	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: ingressName, Namespace: nsName}, ing)
		return err == nil
	})

	if len(ing.Spec.Rules) != 1 {
		t.Fatalf("expected 1 ingress rule, got %d", len(ing.Spec.Rules))
	}
	wantHost := "app." + domain
	if ing.Spec.Rules[0].Host != wantHost {
		t.Errorf("expected host %q, got %q", wantHost, ing.Spec.Rules[0].Host)
	}
	if ing.Spec.Rules[0].HTTP == nil || len(ing.Spec.Rules[0].HTTP.Paths) == 0 {
		t.Fatal("expected HTTP paths")
	}
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend
	if backend.Service.Name != "my-svc" {
		t.Errorf("expected service name my-svc, got %q", backend.Service.Name)
	}
	if backend.Service.Port.Number != 8080 {
		t.Errorf("expected service port 8080, got %d", backend.Service.Port.Number)
	}
	if len(ing.Spec.TLS) != 1 {
		t.Fatalf("expected 1 TLS entry, got %d", len(ing.Spec.TLS))
	}
	wantSecret := fmt.Sprintf("app-%s-%s-tls", tenantName, profileName)
	if ing.Spec.TLS[0].SecretName != wantSecret {
		t.Errorf("expected TLS secret %q, got %q", wantSecret, ing.Spec.TLS[0].SecretName)
	}
}

// TestIngress_CreatesCertificateForTenant: wildcard cert-manager Certificate CR is created.
func TestIngress_CreatesCertificateForTenant(t *testing.T) {
	t.Parallel()
	profileName := "ingress-profile-cert"
	profile := newIngressProfile(profileName, &gentianov1alpha1.IngressSpec{
		ServicePort:   80,
		SubDomain:     "app",
		TLSEnabled:    true,
		ClusterIssuer: "my-issuer",
	})
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenantName := "ingress-cert"
	domain := "ingress-cert.example.com"
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Cert Ingress Co",
			Domain:      domain,
			AdminEmail:  "admin@ingress-cert.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: profileName}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	nsName := fmt.Sprintf("tenant-%s", tenantName)
	certName := fmt.Sprintf("app-%s-%s", tenantName, profileName)

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerCertGVKTest)
	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: certName, Namespace: nsName}, cert)
		return err == nil
	})

	dnsNames, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	wantHost := "app." + domain
	if len(dnsNames) != 1 || dnsNames[0] != wantHost {
		t.Errorf("expected dnsNames=[%q], got %v", wantHost, dnsNames)
	}

	secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	wantCertSecret := fmt.Sprintf("app-%s-%s-tls", tenantName, profileName)
	if secretName != wantCertSecret {
		t.Errorf("expected secretName %q, got %q", wantCertSecret, secretName)
	}

	issuerName, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	if issuerName != "my-issuer" {
		t.Errorf("expected issuerRef.name my-issuer, got %q", issuerName)
	}
	issuerKind, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
	if issuerKind != "ClusterIssuer" {
		t.Errorf("expected issuerRef.kind ClusterIssuer, got %q", issuerKind)
	}
}

// TestIngress_MultipleApps: 2 apps => 2 Ingress resources but only 1 Certificate.
func TestIngress_MultipleApps(t *testing.T) {
	t.Parallel()
	profiles := []string{"ingress-profile-multi-a", "ingress-profile-multi-b"}
	for _, name := range profiles {
		p := newIngressProfile(name, &gentianov1alpha1.IngressSpec{ServicePort: 80, TLSEnabled: true})
		if err := testClient.Create(context.Background(), p); err != nil {
			t.Fatalf("create AppProfile %s: %v", name, err)
		}
		pLocal := p
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), pLocal) })
	}

	tenantName := "ingress-multi"
	domain := "ingress-multi.example.com"
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Multi Ingress Co",
			Domain:      domain,
			AdminEmail:  "admin@ingress-multi.example.com",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "ingress-profile-multi-a"},
				{Profile: "ingress-profile-multi-b"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	nsName := fmt.Sprintf("tenant-%s", tenantName)
	for _, appName := range profiles {
		ingName := fmt.Sprintf("ingress-%s-%s", tenantName, appName)
		ing := &networkingv1.Ingress{}
		waitFor(t, 15*time.Second, func() bool {
			return testClient.Get(context.Background(), types.NamespacedName{Name: ingName, Namespace: nsName}, ing) == nil
		})

		certName := fmt.Sprintf("app-%s-%s", tenantName, appName)
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certManagerCertGVKTest)
		waitFor(t, 15*time.Second, func() bool {
			return testClient.Get(context.Background(), types.NamespacedName{Name: certName, Namespace: nsName}, cert) == nil
		})
	}
}

// TestIngress_DeleteRemovesIngressAndCert: deleting a Tenant removes Ingress and Certificate.
func TestIngress_DeleteRemovesIngressAndCert(t *testing.T) {
	t.Parallel()
	profileName := "ingress-profile-delete"
	profile := newIngressProfile(profileName, &gentianov1alpha1.IngressSpec{ServicePort: 80, TLSEnabled: true})
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenantName := "ingress-delete"
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Delete Ingress Co",
			Domain:         "ingress-delete.example.com",
			AdminEmail:     "admin@ingress-delete.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps:           []gentianov1alpha1.TenantApp{{Profile: profileName}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	nsName := fmt.Sprintf("tenant-%s", tenantName)
	ingressName := fmt.Sprintf("ingress-%s-%s", tenantName, profileName)
	certName := fmt.Sprintf("app-%s-%s", tenantName, profileName)

	ing := &networkingv1.Ingress{}
	waitFor(t, 15*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: ingressName, Namespace: nsName}, ing) == nil
	})

	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: ingressName, Namespace: nsName}, &networkingv1.Ingress{})
		return err != nil
	})

	certObj := &unstructured.Unstructured{}
	certObj.SetGroupVersionKind(certManagerCertGVKTest)
	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: certName, Namespace: nsName}, certObj)
		return err != nil
	})
}
