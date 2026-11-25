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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/homesync"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// The plan names the tenant's home per kind -- the provider it names in
// spec.homes, else the first by name -- at the address the provider's profile
// gives, and every installed app that declares homeSync; a tenant without a
// home sync placed has no plan.
func TestTheHomeSyncsPlanFollowsTheTenant(t *testing.T) {
	dav := func(name string) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		for _, c := range []string{"calendar-store", "contacts-store"} {
			p.Spec.Provides = append(p.Spec.Provides, gentianov1alpha1.ContractRef{Name: c,
				Endpoint: &gentianov1alpha1.ContractEndpoint{Service: name, Port: 8080, Path: "/dav/"}})
		}
		return p
	}
	odoo := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "odoo"}}
	odoo.Spec.HomeSync = &gentianov1alpha1.HomeSyncSpec{
		Endpoint: gentianov1alpha1.ContractEndpoint{Service: "odoo", Port: 8069},
		Calendar: &gentianov1alpha1.HomeSyncMapping{Driver: "odoo", Model: "calendar.event",
			Fields: map[string]string{"summary": "name"}, Filter: &apiextensionsv1.JSON{Raw: []byte(`[["user_id","=","$user"]]`)}},
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	in := netpolicy.BuildInput{
		Namespace: "tenant-acme",
		Apps:      []gentianov1alpha1.TenantApp{{Profile: "odoo"}, {Profile: "nextcloud"}, {Profile: "another-dav"}},
		Placed:    []string{"desktop", gentianov1alpha1.HomeSyncProfile},
		Profiles:  map[string]*gentianov1alpha1.ComponentProfile{"odoo": odoo, "nextcloud": dav("nextcloud"), "another-dav": dav("another-dav")},
	}

	plan := homeSyncPlan(tenant, in)
	if plan == nil || plan.Home.Calendar == nil || plan.Home.Calendar.Profile != "another-dav" ||
		plan.Home.Calendar.URL != "http://another-dav.tenant-acme.svc.cluster.local:8080/dav/" {
		t.Fatalf("without a named home: %+v", plan)
	}
	tenant.Spec.Homes = map[string]string{"calendar-store": "nextcloud", "contacts-store": "not-installed"}
	tenant.Spec.ContactsBook = "shared"
	plan = homeSyncPlan(tenant, in)
	if plan.Home.Calendar.Profile != "nextcloud" || plan.Home.Contacts != nil || plan.ContactsBook != "shared" {
		t.Fatalf("with named homes: %+v", plan.Home)
	}
	if len(plan.Apps) != 1 || plan.Apps[0].Profile != "odoo" || plan.Apps[0].URL != "http://odoo.tenant-acme.svc.cluster.local:8069" {
		t.Fatalf("apps: %+v", plan.Apps)
	}
	m := plan.Apps[0].Kinds[homesync.Calendar]
	if m.Model != "calendar.event" || m.Fields[homesync.Summary] != "name" || string(m.Filter) != `[["user_id","=","$user"]]` {
		t.Fatalf("mapping: %+v", m)
	}

	in.Placed = []string{"desktop"}
	if homeSyncPlan(tenant, in) != nil {
		t.Fatal("a tenant without a home sync has a plan")
	}
}
