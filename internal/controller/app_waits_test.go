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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func waitsScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	return scheme
}

// An uninstall goes through whatever else the tenant is waiting for: the
// Component of an app that left spec.apps is removed before any stage runs,
// while another entry names a profile the cluster does not have.
func TestAnUninstallWaitsForNothing(t *testing.T) {
	ctx := context.Background()
	scheme := waitsScheme()
	tenant := acmeTenantFixture()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "openproject-ce"}, {Profile: "vikunja-ce"}}
	ns := tenantNamespaceName(tenant)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	// Vikunja is uninstalled; an app whose profile is missing is installed.
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{
		{Profile: "openproject-ce"},
		{ProfileRef: &gentianov1alpha1.ProfileReference{Name: "not-on-this-cluster"}},
	}
	r.pruneUninstalledComponents(ctx, tenant)
	if got := componentNames(t, c, ns); got != "openproject-ce" {
		t.Fatalf("components = %q, want only openproject-ce", got)
	}

	// An entry that names no profile could be any Component: nothing goes.
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{}}
	r.pruneUninstalledComponents(ctx, tenant)
	if got := componentNames(t, c, ns); got != "openproject-ce" {
		t.Fatalf("with an entry naming nothing, components = %q", got)
	}
}

// An app waiting for its kernel resources is held back, and only that app:
// it gets no Component while it waits, or keeps the one it has as it is,
// and the tenant's other apps are installed and moved meanwhile.
func TestAWaitingAppIsHeldBackAndTheOthersGoOn(t *testing.T) {
	ctx := context.Background()
	scheme := waitsScheme()
	tenant := acmeTenantFixture()
	first := "sha256:" + strings.Repeat("ab", 32)
	moved := "sha256:" + strings.Repeat("cd", 32)
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "openproject-ce"}, {Profile: "vikunja-ce", Digest: first}}
	ns := tenantNamespaceName(tenant)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	waits := appWaits{}
	waits.add("openproject-ce", "the tenant's Memcached")
	if err := r.ensureAppComponentsHolding(ctx, tenant, waits); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "vikunja-ce" {
		t.Fatalf("while OpenProject waits, components = %q, want vikunja-ce", got)
	}

	// OpenProject's resources are there: it is installed.
	if err := r.ensureAppComponentsHolding(ctx, tenant, nil); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "openproject-ce vikunja-ce" {
		t.Fatalf("components = %q", got)
	}

	// Vikunja waits again and is moved in git: it keeps its build until its
	// resources are there, and is not removed meanwhile.
	tenant.Spec.Apps[1].Digest = moved
	waits = appWaits{}
	waits.add("vikunja-ce", "its PostgreSQL role")
	if err := r.ensureAppComponentsHolding(ctx, tenant, waits); err != nil {
		t.Fatal(err)
	}
	comp := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, types.NamespacedName{Name: "vikunja-ce", Namespace: ns}, comp); err != nil {
		t.Fatalf("a waiting app's Component was removed: %v", err)
	}
	if comp.Spec.ProfileRef.Digest != first {
		t.Fatalf("a waiting app was moved to %q", comp.Spec.ProfileRef.Digest)
	}
	if err := r.ensureAppComponentsHolding(ctx, tenant, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "vikunja-ce", Namespace: ns}, comp); err != nil {
		t.Fatal(err)
	}
	if comp.Spec.ProfileRef.Digest != moved {
		t.Fatalf("once its resources are there the app is still at %q", comp.Spec.ProfileRef.Digest)
	}
}

// A Memcached that is not up holds back the apps that use it, by name, and
// says so; the apps that do not use it are not named.
func TestTheCacheNamesTheAppsWaitingForIt(t *testing.T) {
	ctx := context.Background()
	scheme := waitsScheme()
	memcached := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "openproject-ce"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp},
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineMemcached},
			}},
		},
	}
	plain := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "vikunja-ce"},
		Spec:       gentianov1alpha1.ComponentProfileSpec{Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}},
	}
	tenant := acmeTenantFixture()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "openproject-ce"}, {Profile: "vikunja-ce"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), memcached, plain).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	waits := appWaits{}
	res, err := r.ensureCache(ctx, tenant, waits)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("a cache that is not up asks to be looked at again")
	}
	if !waits.holds("openproject-ce") || waits.holds("vikunja-ce") {
		t.Fatalf("waits = %v, want openproject-ce only", waits)
	}
	if got := waits.describe(); got != "openproject-ce (the tenant's Memcached)" {
		t.Fatalf("describe = %q", got)
	}
}
