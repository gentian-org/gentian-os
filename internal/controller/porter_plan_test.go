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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/porter"
)

// The plan names the tenant's file home where its profile says where a
// person's files are, with the address that opens a file on the home's own
// gateway entry, and every installed app that asks the dialog and is granted
// the file store, by the origins of its pages; a tenant without a porter
// placed has no plan.
func TestThePortersPlanFollowsTheTenant(t *testing.T) {
	gateway := func(name, sub string) gentianov1alpha1.ExposureSpec {
		return gentianov1alpha1.ExposureSpec{Name: name, Surface: gentianov1alpha1.SurfaceGateway, AuthMode: "oidc", SubDomain: sub}
	}
	home := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud"}}
	home.Spec.Provides = []gentianov1alpha1.ContractRef{{
		Name:     "file-store",
		Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "nextcloud", Port: 8080, Path: "/remote.php/dav/"},
		Files: &gentianov1alpha1.FileHomeFacts{
			Root: "/files/{principal}/", IDProperty: "{http://owncloud.org/ns}fileid",
			Open: gentianov1alpha1.FileOpenAddress{Entry: "web", Path: "/f/{id}"},
		},
	}}
	home.Spec.Expose = []gentianov1alpha1.ExposureSpec{gateway("web", "cloud")}
	asks := func(name string, uses ...gentianov1alpha1.FileDialogUse) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Spec.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "file-store", Dialog: uses}}
		p.Spec.Expose = []gentianov1alpha1.ExposureSpec{gateway("web", name), gateway("api", name)}
		return p
	}
	binding := func(consumer string, granted metav1.ConditionStatus) *gentianov1alpha1.IntegrationBinding {
		ib := &gentianov1alpha1.IntegrationBinding{ObjectMeta: metav1.ObjectMeta{Name: consumer + "-file-store"}}
		ib.Spec.Contract = "file-store"
		ib.Spec.Consumer.App = consumer
		ib.Spec.Provider.App = "nextcloud"
		ib.Status.Conditions = []metav1.Condition{{Type: conditionBindingGranted, Status: granted, Reason: "x"}}
		return ib
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	zone := addresses.Zone{Domain: "acme.example.org"}
	in := netpolicy.BuildInput{
		Namespace: "tenant-acme",
		Apps:      []gentianov1alpha1.TenantApp{{Profile: "odoo"}, {Profile: "nextcloud"}, {Profile: "editor"}, {Profile: "plain"}},
		Placed:    []string{"desktop", gentianov1alpha1.PorterProfile},
		Profiles: map[string]*gentianov1alpha1.ComponentProfile{
			"nextcloud": home, "odoo": asks("odoo", gentianov1alpha1.FileDialogPick),
			"editor": asks("editor", gentianov1alpha1.FileDialogOpen, gentianov1alpha1.FileDialogSave),
			"plain":  asks("plain"),
		},
		Bindings: []*gentianov1alpha1.IntegrationBinding{
			binding("odoo", metav1.ConditionTrue), binding("editor", metav1.ConditionFalse), binding("plain", metav1.ConditionTrue),
		},
	}

	plan := porterPlan(tenant, in, zone)
	if plan == nil || plan.Home == nil {
		t.Fatalf("no home: %+v", plan)
	}
	if *plan.Home != (porter.Home{
		Profile: "nextcloud", URL: "http://nextcloud.tenant-acme.svc.cluster.local:8080/remote.php/dav/",
		Root: "/files/{principal}/", IDProperty: "{http://owncloud.org/ns}fileid", Open: "https://cloud.acme.example.org/f/{id}",
	}) {
		t.Fatalf("home: %+v", plan.Home)
	}
	// Granted and asking: odoo. The editor's grant is not given, and plain
	// asks the dialog for nothing.
	if len(plan.Apps) != 1 || plan.Apps[0].Profile != "odoo" || len(plan.Apps[0].Origins) != 1 ||
		plan.Apps[0].Origins[0] != "https://odoo.acme.example.org" || len(plan.Apps[0].Uses) != 1 || plan.Apps[0].Uses[0] != porter.Pick {
		t.Fatalf("apps: %+v", plan.Apps)
	}

	// A home that does not say where a person's files are is not offered.
	home.Spec.Provides[0].Files = nil
	if plan = porterPlan(tenant, in, zone); plan.Home != nil {
		t.Fatalf("a home with no facts: %+v", plan.Home)
	}
	in.Placed = []string{"desktop"}
	if porterPlan(tenant, in, zone) != nil {
		t.Fatal("a tenant without a porter has a plan")
	}
}
