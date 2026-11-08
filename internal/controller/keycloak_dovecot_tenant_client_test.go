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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func dovecotClientFixture(name string, labels map[string]string, ready bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(keycloakClientGVK)
	obj.SetName(name)
	obj.SetLabels(labels)
	status := "False"
	if ready {
		status = "True"
	}
	_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": status},
	}, "status", "conditions")
	return obj
}

// A tenant waits for gentian-dovecot in the realm its identity lives in. For
// the tenant that adopts the kernel realm that is the client cluster-default
// composes, found by its label: tenant-default composes none of an adopted
// realm's state, so a Client named after the tenant never exists, and waiting
// for one held Tenant/platform at MailReady=False for good.
func TestATenantWaitsForTheDovecotClientOfItsOwnRealm(t *testing.T) {
	kernelLabels := map[string]string{kernelDovecotClientPurposeLabel: kernelDovecotClientPurpose}
	kernelClient := func(ready bool) *unstructured.Unstructured {
		return dovecotClientFixture("dev-cluster-x7k2p-dovecot-oidc-client", kernelLabels, ready)
	}
	acmeClient := func(ready bool) *unstructured.Unstructured {
		return dovecotClientFixture(tenantDovecotOIDCClientName("acme"), nil, ready)
	}
	for _, tc := range []struct {
		name    string
		adopts  bool
		objects []*unstructured.Unstructured
		want    bool
	}{
		{"the adopting tenant, no client composed yet", true, nil, false},
		{"the adopting tenant, the kernel realm's client not Ready", true, []*unstructured.Unstructured{kernelClient(false)}, false},
		{"the adopting tenant, the kernel realm's client Ready", true, []*unstructured.Unstructured{kernelClient(true)}, true},
		{"the adopting tenant is not answered by another tenant's client", true, []*unstructured.Unstructured{acmeClient(true)}, false},
		{"a tenant with its own realm, its client not composed yet", false, nil, false},
		{"a tenant with its own realm, its client Ready", false, []*unstructured.Unstructured{acmeClient(true)}, true},
		{"a tenant with its own realm is not answered by the kernel realm's client", false, []*unstructured.Unstructured{kernelClient(true)}, false},
	} {
		b := fake.NewClientBuilder().WithScheme(runtime.NewScheme())
		for _, o := range tc.objects {
			b = b.WithObjects(o)
		}
		r := &TenantReconciler{Client: b.Build(), KernelRealm: "kernel"}
		tenant := acmeTenantFixture()
		if tc.adopts {
			tenant = platformTenantFixture()
		}
		got, err := r.dovecotTenantClientReady(context.Background(), tenant)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: ready=%v, want %v", tc.name, got, tc.want)
		}
	}
}
