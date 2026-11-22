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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func trustingTenant(delegated bool, domains ...string) *gentianov1alpha1.Tenant {
	return &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: gentianov1alpha1.TenantSpec{
			TrustedDomains: &gentianov1alpha1.TenantTrustedDomains{Delegated: delegated, Domains: domains},
		},
	}
}

// A change to what the tenant's own list answers wakes its apps; any other
// change to the tenant, or a list that answers nothing before and after,
// does not.
func TestATenantsTrustedDomainsWakeItsAppsWhenWhatTheyAnswerChanges(t *testing.T) {
	changed := trustedDomainsChanged()
	for _, tc := range []struct {
		name          string
		before, after *gentianov1alpha1.Tenant
		want          bool
	}{
		{"a domain removed", trustingTenant(true, "github.com"), trustingTenant(true), true},
		{"a domain added", trustingTenant(true), trustingTenant(true, "example.org"), true},
		{"the list taken back", trustingTenant(true, "github.com"), trustingTenant(false, "github.com"), true},
		{"the list delegated", trustingTenant(false, "github.com"), trustingTenant(true, "github.com"), true},
		{"nothing it answers", trustingTenant(true, "github.com"), trustingTenant(true, "github.com"), false},
		{"a list not delegated", trustingTenant(false), trustingTenant(false, "github.com"), false},
	} {
		if got := changed.Update(event.UpdateEvent{ObjectOld: tc.before, ObjectNew: tc.after}); got != tc.want {
			t.Errorf("%s: woken %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Only the tenant's own Components whose profile asks for egress are woken.
func TestATenantsTrustedDomainsWakeOnlyItsAppsThatAskForEgress(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	profile := func(name string, egress bool) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if egress {
			p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
				Egress: []gentianov1alpha1.EgressRequest{{Name: "module-source", Hosts: []string{"github.com"}}},
			}}
		}
		return p
	}
	component := func(namespace, name, profile string) *gentianov1alpha1.Component {
		return &gentianov1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       gentianov1alpha1.ComponentSpec{ProfileRef: gentianov1alpha1.ProfileRef{Name: profile}},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		profile("odoo", true), profile("wiki", false),
		component("tenant-acme", "odoo", "odoo"),
		component("tenant-acme", "wiki", "wiki"),
		component("tenant-other", "odoo", "odoo"),
	).Build()

	got := egressAskingComponents(context.Background(), c, client.InNamespace(trustingTenant(true).NamespaceName()))
	if len(got) != 1 || got[0].Namespace != "tenant-acme" || got[0].Name != "odoo" {
		t.Fatalf("woken %v, want tenant-acme/odoo alone", got)
	}
}
