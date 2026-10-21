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
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestTheTenantsThePlatformsAdministratorsEnterAreProjected(t *testing.T) {
	ctx := context.Background()
	platform := &gentianov1alpha1.Tenant{}
	platform.Name = "platform"
	platform.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	acme := &gentianov1alpha1.Tenant{}
	acme.Name = "acme"
	acme.Spec.PlatformAccess = &gentianov1alpha1.TenantPlatformAccess{AdminsEnter: true}
	beta := &gentianov1alpha1.Tenant{}
	beta.Name = "beta"

	build := func(r *TenantReconciler, target *gentianov1alpha1.Tenant) map[string]interface{} {
		xr, err := r.buildXTenant(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		spec, _ := xr.Object["spec"].(map[string]interface{})
		return spec
	}
	objs := []client.Object{platform.DeepCopy(), acme.DeepCopy(), beta.DeepCopy()}
	r := &TenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(objs...).Build(),
		KernelDomain: "k.example", TenancyMode: "multi", KernelRealm: "kernel",
	}

	spec := build(r, platform)
	enter, _ := spec["kernelEnter"].([]interface{})
	if len(enter) != 1 || enter[0].(map[string]interface{})["tenant"] != "acme" || enter[0].(map[string]interface{})["realm"] != "acme" {
		t.Fatalf("kernelEnter = %v: the one tenant that opened itself", spec["kernelEnter"])
	}
	if spec := build(r, acme); spec["kernelBroker"] != true {
		t.Fatalf("acme's composite: kernelBroker=%v", spec["kernelBroker"])
	}
	spec = build(r, beta)
	for _, k := range []string{"kernelBroker", "kernelEnter"} {
		if _, stated := spec[k]; stated {
			t.Fatalf("beta carries %s: %v", k, spec[k])
		}
	}

	// A cluster that names no kernel realm brokers nothing.
	r.KernelRealm = ""
	if spec := build(r, acme); spec["kernelBroker"] != nil {
		t.Fatalf("a broker to a kernel realm nobody named: %v", spec["kernelBroker"])
	}

	// The XRD declares what is projected, or the API server prunes it in
	// silence and the Composition composes nothing.
	xrd, err := os.ReadFile("../../crossplane/xrds/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"kernelEnter:", "kernelBroker:"} {
		if !strings.Contains(string(xrd), field) {
			t.Errorf("the Tenant XRD does not declare %s", field)
		}
	}
}

func TestTheEnterWatchLetsThroughWhatChangesIt(t *testing.T) {
	acme := &gentianov1alpha1.Tenant{}
	acme.Name = "acme"
	opened := acme.DeepCopy()
	opened.Spec.PlatformAccess = &gentianov1alpha1.TenantPlatformAccess{AdminsEnter: true}
	quota := acme.DeepCopy()
	quota.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "notes"}}

	tenants := tenantEnterFactsChanged()
	if !tenants.Update(event.UpdateEvent{ObjectOld: acme, ObjectNew: opened}) {
		t.Error("a tenant opening itself to the platform's administrators does not reach the kernel realm's tenant")
	}
	if tenants.Update(event.UpdateEvent{ObjectOld: acme, ObjectNew: quota}) {
		t.Error("an installed app wakes the kernel realm's tenant")
	}

	// The map: whatever changed, the kernel realm's tenant is the one redone.
	platform := &gentianov1alpha1.Tenant{}
	platform.Name = "platform"
	platform.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	c := fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(platform, acme).Build()
	queue := &recordingQueue{}
	tenantsAdoptingKernelRealm(c, "kernel").Update(context.Background(), event.UpdateEvent{ObjectOld: acme, ObjectNew: opened}, queue)
	if len(queue.added) != 1 || queue.added[0].Name != "platform" {
		t.Fatalf("redone: %v, want the kernel realm's tenant alone", queue.added)
	}
}

// recordingQueue is a workqueue that remembers what was added to it.
type recordingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	added []reconcile.Request
}

func (q *recordingQueue) Add(item reconcile.Request) { q.added = append(q.added, item) }
