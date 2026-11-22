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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A contract two installed apps provide is bound to the one the tenant names
// its home for that kind of data; one it names that is not installed binds
// nobody; and one it does not name is bound to the first provider by name, on
// every pass alike.
func TestAContractIsBoundToTheTenantsHome(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	provides := func(name string, contracts ...string) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		for _, c := range contracts {
			p.Spec.Provides = append(p.Spec.Provides, gentianov1alpha1.ContractRef{Name: c})
		}
		return p
	}
	consumer := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "odoo"}}
	consumer.Spec.Integrations = []gentianov1alpha1.IntegrationRef{
		{Contract: "calendar-store", AsPerson: true},
		{Contract: "contacts-store", AsPerson: true},
		{Contract: "file-store"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		consumer,
		provides("nextcloud", "calendar-store", "contacts-store", "file-store"),
		provides("another-dav", "calendar-store", "contacts-store", "file-store"),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	providers := func(homes map[string]string) map[string]string {
		t.Helper()
		tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
		tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "odoo"}, {Profile: "nextcloud"}, {Profile: "another-dav"}}
		tenant.Spec.Homes = homes
		bindings, err := r.collectDesiredIntegrationBindings(context.Background(), tenant)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, ib := range bindings {
			out[ib.Spec.Contract] = ib.Spec.Provider.App
		}
		return out
	}

	got := providers(map[string]string{"calendar-store": "nextcloud", "contacts-store": "not-installed"})
	if got["calendar-store"] != "nextcloud" {
		t.Errorf("calendar-store is bound to %q, want the tenant's home nextcloud", got["calendar-store"])
	}
	if p, ok := got["contacts-store"]; ok {
		t.Errorf("contacts-store is bound to %q, want nobody: its home is not installed", p)
	}
	for i := 0; i < 5; i++ {
		if p := providers(nil)["file-store"]; p != "another-dav" {
			t.Fatalf("file-store is bound to %q, want the first provider by name, another-dav", p)
		}
	}
}
