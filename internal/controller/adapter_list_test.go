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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

func adapterListProfile(classes []gentianov1alpha1.ComponentClass, mutate func(*gentianov1alpha1.ComponentProfile)) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{Classes: classes}}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// Every component that may call the tenant's adapters is told which there
// are: the installed adapters that say where they answer, each with its
// address inside the tenant and the app it adapts. A caller the platform
// places is told like an installed one; a component that may not call them
// is told nothing; and the list follows what is installed, down to the list
// of a component that is a caller no more.
func TestEveryCallerIsToldTheTenantsAdapters(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	app := []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}
	adapter := []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassAdapter}
	speaks := func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
			Vouching: &gentianov1alpha1.VouchingRequirement{},
		}}
	}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"odoo":      adapterListProfile(app, nil),
		"chaperone": adapterListProfile(app, speaks),
		"usher":     adapterListProfile(app, speaks),
		"desktop":   adapterListProfile(app, nil),
		"odoo-adapter": adapterListProfile(adapter, func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Adapter = &gentianov1alpha1.AdapterSpec{
				Adapts:   &gentianov1alpha1.AdaptedApp{Profile: "odoo"},
				Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "odoo-adapter", Port: 8765, Path: "/mcp"},
			}
		}),
		// Installed, and silent about where it answers: on nobody's list.
		"silent-adapter": adapterListProfile(adapter, func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Adapter = &gentianov1alpha1.AdapterSpec{Adapts: &gentianov1alpha1.AdaptedApp{Profile: "odoo"}}
		}),
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "uid-acme"}}
	in := netpolicy.BuildInput{
		TenantName: "acme", Namespace: "tenant-acme", Profiles: profiles,
		Apps: []gentianov1alpha1.TenantApp{
			{Profile: "odoo"}, {Profile: "chaperone"}, {Profile: "odoo-adapter"}, {Profile: "silent-adapter"},
		},
		Placed: []string{"usher", "desktop"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme}

	list := func(component string) (string, bool) {
		t.Helper()
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: AdapterListName(component)}, cm)
		if errors.IsNotFound(err) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != "acme" {
			t.Fatalf("%s is owned by %+v, want the tenant", cm.Name, cm.OwnerReferences)
		}
		return cm.Data[AdapterListKey], true
	}

	if err := tr.ensureAdapterLists(ctx, tenant, in); err != nil {
		t.Fatal(err)
	}
	const want = `{"odoo-adapter":{"url":"http://odoo-adapter.tenant-acme.svc.cluster.local:8765/mcp","app":"odoo"}}`
	for _, caller := range []string{"chaperone", "usher"} {
		if got, ok := list(caller); !ok || got != want {
			t.Fatalf("%s is told %q (present %v), want %s", caller, got, ok, want)
		}
	}
	for _, other := range []string{"desktop", "odoo", "odoo-adapter"} {
		if _, ok := list(other); ok {
			t.Fatalf("%s may not call adapters and was given a list", other)
		}
	}

	// The adapter is uninstalled and the placed caller withdrawn: the one
	// caller left is told there is none, and the other's list is gone.
	in.Apps = []gentianov1alpha1.TenantApp{{Profile: "odoo"}, {Profile: "chaperone"}}
	in.Placed = []string{"desktop"}
	if err := tr.ensureAdapterLists(ctx, tenant, in); err != nil {
		t.Fatal(err)
	}
	if got, ok := list("chaperone"); !ok || got != "{}" {
		t.Fatalf("chaperone is told %q (present %v), want an empty list", got, ok)
	}
	if _, ok := list("usher"); ok {
		t.Fatal("a component that is a caller no more kept its list")
	}
}
