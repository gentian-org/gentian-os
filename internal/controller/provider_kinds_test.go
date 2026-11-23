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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// The objects the operator hands provider-kubernetes for a tenant (its
// databases, its memcached, its integration bindings) are created as the
// provider's ServiceAccount, whose ClusterRole is generated from
// provider-kubernetes-kinds.yaml. No render fixture shows them, so
// lint-provider-rbac cannot see them: a kind missing from the list was found
// only when the API server refused it on a live cluster (the memcached
// Deployment, #1294). This builds them for a tenant whose apps ask for every
// one and fails on a kind the list does not grant.
func TestDataPlaneObjectKindsAreGranted(t *testing.T) {
	ctx := context.Background()

	raw, err := os.ReadFile("../../crossplane/providers/provider-kubernetes-kinds.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Kinds []struct {
			Group     string   `json:"group"`
			Resources []string `json:"resources"`
		} `json:"kinds"`
	}
	if err := yaml.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for _, g := range list.Kinds {
		for _, r := range g.Resources {
			granted[g.Group+"/"+r] = true
		}
	}

	profile := func(name string, mutate func(*gentianov1alpha1.ComponentProfileSpec)) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		mutate(&p.Spec)
		return p
	}
	wiki := profile("wiki", func(s *gentianov1alpha1.ComponentProfileSpec) {
		s.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
			Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL},
			Cache:    &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineMemcached},
		}}
		s.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "file-store", Provider: "drive"}}
	})
	drive := profile("drive", func(s *gentianov1alpha1.ComponentProfileSpec) {
		s.Provides = []gentianov1alpha1.ContractRef{{
			Name: "file-store", Protocol: "http",
			Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "drive", Port: 80},
		}}
	})
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{{Profile: "wiki"}, {Profile: "drive"}}},
	}

	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(wiki, drive, tenant).Build()
	r := &TenantReconciler{Client: c, Scheme: s, Seeder: secrets.NewSeeder(memVault{}, secrets.NewDeriver("unit-test-master"))}

	objects, err := r.buildDataPlaneObjects(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}

	// Every kind the operator can hand over, so a builder that stops emitting
	// one does not pass this by checking less.
	want := map[string]bool{"Database": false, "Deployment": false, "Service": false, "IntegrationBinding": false}
	for _, obj := range objects {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if _, ok := want[gvk.Kind]; ok {
			want[gvk.Kind] = true
		}
		resource := gvk.Group + "/" + strings.ToLower(gvk.Kind) + "s"
		if !granted[resource] {
			t.Errorf("the operator hands provider-kubernetes a %s (%s), which provider-kubernetes-kinds.yaml does not grant", gvk.Kind, resource)
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("the fixture tenant produced no %s; it is meant to exercise every kind", kind)
		}
	}
}
