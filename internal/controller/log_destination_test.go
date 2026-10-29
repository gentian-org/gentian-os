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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const observabilityNS = "kernel-observability"

func logDestinationProfile(name string, trust gentianov1alpha1.TrustTier) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	profile.Spec.TrustTier = trust
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{
			LogDestination: &gentianov1alpha1.LogDestinationRequirement{Service: "evidence-api", Port: 8443},
		},
	}
	return profile
}

func logDestinationCert(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerCertGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: logDestinationCertificate, Namespace: observabilityNS}, cert); err != nil {
		t.Fatalf("the certificate: %v", err)
	}
	return cert
}

func issue(t *testing.T, c client.Client) {
	t.Helper()
	if err := c.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: logDestinationCertificate, Namespace: observabilityNS},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("crt"), "tls.key": []byte("key"), "ca.crt": []byte("ca")},
	}); err != nil {
		t.Fatal(err)
	}
}

// The platform tenant's component that declares it is given a certificate
// for its Service's names from the issuer that signs only those, a copy of
// it in its own namespace once issued, and a way in for the forwarder's pods
// on its one port.
func TestTheLogDestinationIsGivenItsCertificateAndTheForwardersWayIn(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	comp := rightsComponent("platform", "evidence-store-ee")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	profile := logDestinationProfile("evidence-store-ee", gentianov1alpha1.TrustTierPlatform)

	if err := r.ensureLogDestination(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	cert := logDestinationCert(t, c)
	names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if len(names) != 2 || names[0] != "evidence-api.tenant-platform.svc" || names[1] != "evidence-api.tenant-platform.svc.cluster.local" {
		t.Fatalf("names %v", names)
	}
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	if issuer != "gentian-ca-platform-services" {
		t.Fatalf("issuer %q", issuer)
	}
	if c := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionLogDestination); c == nil || c.Reason != "Issuing" {
		t.Fatalf("before issue: %+v", c)
	}

	policy := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "log-destination-evidence-store-ee", Namespace: comp.Namespace}, policy); err != nil {
		t.Fatal(err)
	}
	in := policy.Spec.Ingress
	if len(in) != 1 || len(in[0].Ports) != 1 || in[0].Ports[0].Port.IntValue() != 8443 ||
		in[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != observabilityNS ||
		in[0].From[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "vector" {
		t.Fatalf("ingress %+v", in)
	}

	issue(t, c)
	if err := r.ensureLogDestination(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	copied := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "log-destination-tls-evidence-store-ee", Namespace: comp.Namespace}, copied); err != nil {
		t.Fatal(err)
	}
	if string(copied.Data["tls.key"]) != "key" || string(copied.Data["ca.crt"]) != "ca" || !metav1.IsControlledBy(copied, comp) {
		t.Fatalf("copy %+v", copied)
	}
	if c := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionLogDestination); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("after issue: %+v", c)
	}

	// Renewed: the copy follows.
	issued := &corev1.Secret{}
	_ = c.Get(ctx, types.NamespacedName{Name: logDestinationCertificate, Namespace: observabilityNS}, issued)
	issued.Data["tls.crt"] = []byte("renewed")
	if err := c.Update(ctx, issued); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureLogDestination(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(copied), copied)
	if string(copied.Data["tls.crt"]) != "renewed" {
		t.Fatal("the copy did not follow the renewal")
	}

	// Removed: everything goes, in both namespaces.
	if err := r.deleteLogDestination(ctx, comp); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: logDestinationCertificate, Namespace: observabilityNS}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "log-destination-tls-evidence-store-ee", Namespace: comp.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "log-destination-evidence-store-ee", Namespace: comp.Namespace}},
	} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); !errors.IsNotFound(err) {
			t.Errorf("%s/%s is still there", obj.GetNamespace(), obj.GetName())
		}
	}
	cert = &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerCertGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: logDestinationCertificate, Namespace: observabilityNS}, cert); !errors.IsNotFound(err) {
		t.Error("the certificate is still there")
	}
}

// Outside platform trust, or outside the platform tenant, the declaration
// does nothing: no certificate, no way in.
func TestOnlyThePlatformTenantsComponentIsALogDestination(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	for name, tc := range map[string]struct {
		tenant string
		trust  gentianov1alpha1.TrustTier
	}{
		"another tenant":          {"acme", gentianov1alpha1.TrustTierPlatform},
		"short of platform trust": {"platform", gentianov1alpha1.TrustTierCertified},
	} {
		comp := rightsComponent(tc.tenant, "evidence-store-ee")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp).Build()
		r := &ComponentReconciler{Client: c, Scheme: scheme}
		if err := r.ensureLogDestination(ctx, comp, logDestinationProfile("evidence-store-ee", tc.trust)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certManagerCertGVK)
		if err := c.Get(ctx, types.NamespacedName{Name: logDestinationCertificate, Namespace: observabilityNS}, cert); !errors.IsNotFound(err) {
			t.Errorf("%s: a certificate was asked for", name)
		}
		policies := &networkingv1.NetworkPolicyList{}
		_ = c.List(ctx, policies)
		if len(policies.Items) != 0 {
			t.Errorf("%s: a way in was opened", name)
		}
	}
}

// A cluster has one log destination: a second component that declares it is
// told which one is, and is given nothing.
func TestASecondLogDestinationIsHeld(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	first, second := rightsComponent("platform", "evidence-store-ee"), rightsComponent("platform", "other-store")
	firstProfile := logDestinationProfile("evidence-store-ee", gentianov1alpha1.TrustTierPlatform)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second, firstProfile).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	if err := r.ensureLogDestination(ctx, first, firstProfile); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureLogDestination(ctx, second, logDestinationProfile("other-store", gentianov1alpha1.TrustTierPlatform)); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(second.Status.Conditions, ConditionLogDestination)
	if cond == nil || cond.Reason != "AnotherDestination" {
		t.Fatalf("second: %+v", cond)
	}
	if cert := logDestinationCert(t, c); cert.GetLabels()[logDestinationComponentLabel] != "evidence-store-ee" {
		t.Fatal("the second took the certificate")
	}
	if err := r.deleteLogDestination(ctx, second); err != nil {
		t.Fatal(err)
	}
	logDestinationCert(t, c) // the first's stays
}
