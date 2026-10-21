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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// hubClaim is a Cluster claim naming security officers, as the director
// writes it.
func hubClaim(secondFactor string, officers ...map[string]interface{}) *unstructured.Unstructured {
	claim := clusterClaim("")
	list := make([]interface{}, 0, len(officers))
	for _, o := range officers {
		list = append(list, o)
	}
	_ = unstructured.SetNestedSlice(claim.Object, list, "spec", "platformAccess", "securityOfficers")
	if secondFactor != "" {
		_ = unstructured.SetNestedField(claim.Object, secondFactor, "spec", "platformAccess", "brokeredSecondFactor")
	}
	return claim
}

func officer(tenant, subject, username, email string) map[string]interface{} {
	return map[string]interface{}{"tenant": tenant, "subject": subject, "username": username, "email": email}
}

func TestTheHubIsProjectedFromTheClaimOntoTheKernelRealmsTenant(t *testing.T) {
	ctx := context.Background()
	platform := &gentianov1alpha1.Tenant{}
	platform.Name = "platform"
	platform.Spec.DisplayName = "Platform"
	platform.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	acme := &gentianov1alpha1.Tenant{}
	acme.Name = "acme"
	acme.Spec.DisplayName = "Acme AG"
	acme.Spec.PlatformAccess = &gentianov1alpha1.TenantPlatformAccess{AdminsEnter: true}
	beta := &gentianov1alpha1.Tenant{}
	beta.Name = "beta"

	build := func(target *gentianov1alpha1.Tenant, objs ...client.Object) map[string]interface{} {
		r := &TenantReconciler{
			Client:       fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(objs...).Build(),
			KernelDomain: "k.example", TenancyMode: "multi", KernelRealm: "kernel",
		}
		xr, err := r.buildXTenant(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		spec, _ := xr.Object["spec"].(map[string]interface{})
		return spec
	}

	claim := hubClaim("",
		officer("acme", "7d0f3c2e", "it@acme.example", "it@acme.example"),
		officer("acme", "0a1b2c3d", "ciso@acme.example", ""),
		officer("acme", "7d0f3c2e", "it@acme.example", "it@acme.example"), // the same twice: once
		officer("gone", "1111", "x@gone.example", ""),                    // no such tenant
		officer("platform", "2222", "admin@k.example", ""),                // the kernel realm's own
		officer("beta", "not a subject", "x@beta.example", ""),            // refused by shape
		officer("beta", "3333", "", ""),                                   // no login
	)

	spec := build(platform, platform.DeepCopy(), acme.DeepCopy(), beta.DeepCopy(), claim)
	hub, _ := spec["hub"].(map[string]interface{})
	if hub == nil {
		t.Fatalf("the kernel realm's tenant carries no hub: %v", spec)
	}
	if hub["secondFactor"] != "required" || hub["securityGroup"] != "gentian:platform:security" {
		t.Fatalf("the defaults: %v", hub)
	}
	spokes, _ := hub["spokes"].([]interface{})
	if len(spokes) != 1 {
		t.Fatalf("spokes = %v: acme alone has holders that can be composed", hub["spokes"])
	}
	s := spokes[0].(map[string]interface{})
	if s["tenant"] != "acme" || s["realm"] != "acme" || s["displayName"] != "Acme AG" {
		t.Fatalf("spoke = %v", s)
	}
	holders := s["holders"].([]interface{})
	if len(holders) != 2 {
		t.Fatalf("holders = %v: two distinct subjects", holders)
	}
	first := holders[0].(map[string]interface{})
	if first["subject"] != "0a1b2c3d" || first["username"] != "ciso@acme.example" || first["email"] != "" {
		t.Fatalf("holders are sorted by subject and carry what the claim says: %v", holders)
	}
	enter, _ := hub["enter"].([]interface{})
	if len(enter) != 1 || enter[0].(map[string]interface{})["tenant"] != "acme" || enter[0].(map[string]interface{})["realm"] != "acme" {
		t.Fatalf("enter = %v: the one tenant that opened itself", hub["enter"])
	}
	if _, stated := spec["hubSpoke"]; stated {
		t.Fatalf("the kernel realm's tenant is nobody's spoke: %v", spec["hubSpoke"])
	}

	// The source tenant: a spoke, and open to the platform's administrators.
	spec = build(acme, platform.DeepCopy(), acme.DeepCopy(), beta.DeepCopy(), claim)
	if spec["hubSpoke"] != true || spec["kernelBroker"] != true {
		t.Fatalf("acme's composite: hubSpoke=%v kernelBroker=%v", spec["hubSpoke"], spec["kernelBroker"])
	}
	if _, stated := spec["hub"]; stated {
		t.Fatalf("a source tenant carries no hub: %v", spec["hub"])
	}
	// A tenant with no holder that can be composed, and that did not open
	// itself, composes as it did before the hub existed.
	spec = build(beta, platform.DeepCopy(), acme.DeepCopy(), beta.DeepCopy(), claim)
	for _, k := range []string{"hubSpoke", "kernelBroker", "hub"} {
		if _, stated := spec[k]; stated {
			t.Fatalf("beta carries %s: %v", k, spec[k])
		}
	}

	// The second factor follows the claim, and only "off" switches it off.
	spec = build(platform, platform.DeepCopy(), acme.DeepCopy(), hubClaim("off", officer("acme", "7d0f3c2e", "it@acme.example", "")))
	if hub, _ := spec["hub"].(map[string]interface{}); hub["secondFactor"] != "off" {
		t.Fatalf("secondFactor = %v", hub["secondFactor"])
	}
	spec = build(platform, platform.DeepCopy(), acme.DeepCopy(), hubClaim("anything-else", officer("acme", "7d0f3c2e", "it@acme.example", "")))
	if hub, _ := spec["hub"].(map[string]interface{}); hub["secondFactor"] != "required" {
		t.Fatalf("secondFactor = %v: anything but off is required", hub["secondFactor"])
	}

	// Without a claim: an empty hub on the kernel realm's tenant (its
	// default groups are still declared empty), and nothing anywhere else.
	spec = build(platform, platform.DeepCopy(), acme.DeepCopy())
	hub, _ = spec["hub"].(map[string]interface{})
	if hub == nil || len(hub["spokes"].([]interface{})) != 0 || len(hub["enter"].([]interface{})) != 1 {
		t.Fatalf("without a claim: %v", spec["hub"])
	}
	spec = build(acme, platform.DeepCopy(), acme.DeepCopy())
	if _, stated := spec["hubSpoke"]; stated {
		t.Fatalf("a spoke without a claim: %v", spec["hubSpoke"])
	}

	// A cluster that names no kernel realm brokers nothing.
	r := &TenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(platform.DeepCopy(), acme.DeepCopy(), claim).Build(),
		KernelDomain: "k.example", TenancyMode: "multi",
	}
	xr, err := r.buildXTenant(ctx, acme)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ = xr.Object["spec"].(map[string]interface{})
	if _, stated := spec["kernelBroker"]; stated {
		t.Fatalf("a broker to a kernel realm nobody named: %v", spec["kernelBroker"])
	}

	// The XRD declares what is projected, or the API server prunes it in
	// silence and the Composition composes nothing.
	xrd, err := os.ReadFile("../../crossplane/xrds/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"hub:", "hubSpoke:", "kernelBroker:", "secondFactor:", "securityGroup:", "spokes:", "holders:", "enter:"} {
		if !strings.Contains(string(xrd), field) {
			t.Errorf("the Tenant XRD does not declare %s", field)
		}
	}
	cluster, err := os.ReadFile("../../crossplane/xrds/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"platformAccess:", "brokeredSecondFactor:", "securityOfficers:"} {
		if !strings.Contains(string(cluster), field) {
			t.Errorf("the Cluster XRD does not declare %s", field)
		}
	}
}

func TestTheHubWatchesLetThroughWhatChangesIt(t *testing.T) {
	acme := &gentianov1alpha1.Tenant{}
	acme.Name = "acme"
	opened := acme.DeepCopy()
	opened.Spec.PlatformAccess = &gentianov1alpha1.TenantPlatformAccess{AdminsEnter: true}
	renamed := acme.DeepCopy()
	renamed.Spec.DisplayName = "Acme AG"
	quota := acme.DeepCopy()
	quota.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "notes"}}

	tenants := tenantHubFactsChanged()
	if !tenants.Update(event.UpdateEvent{ObjectOld: acme, ObjectNew: opened}) {
		t.Error("a tenant opening itself to the platform's administrators does not reach the hub")
	}
	if !tenants.Update(event.UpdateEvent{ObjectOld: acme, ObjectNew: renamed}) {
		t.Error("a renamed tenant does not reach the hub, which shows its name on the sign-in page")
	}
	if tenants.Update(event.UpdateEvent{ObjectOld: acme, ObjectNew: quota}) {
		t.Error("an installed app wakes the hub")
	}

	claims := platformAccessChanged()
	before := hubClaim("", officer("acme", "1", "it@acme.example", ""))
	after := hubClaim("", officer("acme", "1", "it@acme.example", ""), officer("acme", "2", "ciso@acme.example", ""))
	if !claims.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Error("a new holder does not reach the hub")
	}
	off := hubClaim("off", officer("acme", "1", "it@acme.example", ""))
	if !claims.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: off}) {
		t.Error("the second factor switched off does not reach the hub")
	}
	store := hubClaim("", officer("acme", "1", "it@acme.example", ""))
	_ = unstructured.SetNestedField(store.Object, "https://store.example.org", "spec", "catalogue", "storeUrl")
	if claims.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: store}) {
		t.Error("a store named on the claim wakes the hub")
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
