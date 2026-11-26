/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// On a Gardener shoot the tenant's wildcard is asked of the shoot's
// certificate service (os #1376): cert-manager there has no DNS-01 issuer,
// and the one its name would point at does not exist.
func TestATenantWildcardOnGardenerIsTheShootsCertificate(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	obj := buildTenantWildcardCertificate(tenant, "tenant-demo", "tenant-demo-wildcard",
		"tenant-demo-wildcard-tls", "demo.example.test", "letsencrypt-dns01-gardener", EdgeCertificatesGardener)

	if obj.GroupVersionKind() != gardenerCertGVK {
		t.Fatalf("kind = %v, want %v", obj.GroupVersionKind(), gardenerCertGVK)
	}
	cn, _, _ := unstructured.NestedString(obj.Object, "spec", "commonName")
	if cn != "*.demo.example.test" {
		t.Fatalf("commonName = %q", cn)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "dnsNames"); found {
		t.Fatal("dnsNames set for a single name: the apex must stay out (see buildTenantWildcardCertificate)")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "issuerRef"); found {
		t.Fatal("a cert-manager issuer named on Gardener's certificate")
	}
	ref, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "secretRef")
	if ref["name"] != "tenant-demo-wildcard-tls" || ref["namespace"] != "tenant-demo" {
		t.Fatalf("secretRef = %v", ref)
	}
	if obj.GetLabels()[tenantLabel] != "demo" {
		t.Fatalf("labels = %v", obj.GetLabels())
	}
}

func TestAnEdgeCertificateIsCertManagersByDefault(t *testing.T) {
	t.Parallel()
	obj := buildEdgeCertificate("", "turn", "edge", "turn-tls", []string{"turn.example.test"}, "issuer")
	if obj.GroupVersionKind() != certManagerCertGVK {
		t.Fatalf("kind = %v", obj.GroupVersionKind())
	}
	names, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "dnsNames")
	issuer, _, _ := unstructured.NestedString(obj.Object, "spec", "issuerRef", "name")
	secret, _, _ := unstructured.NestedString(obj.Object, "spec", "secretName")
	if len(names) != 1 || names[0] != "turn.example.test" || issuer != "issuer" || secret != "turn-tls" {
		t.Fatalf("spec = %v", obj.Object["spec"])
	}
}

func TestAGardenerCertificateListsFurtherNamesBesideItsSubject(t *testing.T) {
	t.Parallel()
	obj := buildEdgeCertificate(EdgeCertificatesGardener, "k", "edge", "k-tls", []string{"example.test", "*.example.test"}, "")
	cn, _, _ := unstructured.NestedString(obj.Object, "spec", "commonName")
	names, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "dnsNames")
	if cn != "example.test" || len(names) != 1 || names[0] != "*.example.test" {
		t.Fatalf("spec = %v", obj.Object["spec"])
	}
}
