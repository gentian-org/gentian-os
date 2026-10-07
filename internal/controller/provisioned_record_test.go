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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

func storesProfile(name string, engine gentianov1alpha1.DatabaseEngine, s3, redis bool) *gentianov1alpha1.ComponentProfile {
	services := &gentianov1alpha1.ServiceRequirements{}
	if engine != "" {
		services.Database = &gentianov1alpha1.DatabaseRequirement{Engine: engine}
	}
	if s3 {
		services.Storage = &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{}}
	}
	if redis {
		services.Cache = &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis}
	}
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gentianov1alpha1.ComponentProfileSpec{Requires: &gentianov1alpha1.RequirementSpec{Services: services}},
	}
}

// An app uninstalled long ago -- its setup Jobs expired, the app gone from
// the tenant's manifest -- still has its stores, and the deletion of the
// tenant has to find every one of them. It used to find a PostgreSQL
// database and nothing else.
func TestTenantDeleteFindsTheStoresOfAnUninstalledApp(t *testing.T) {
	ctx := context.Background()
	scheme := deleteGapsScheme()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps:           []gentianov1alpha1.TenantApp{{Profile: "shop"}, {Profile: "wiki"}},
		},
	}
	shop := storesProfile("shop", gentianov1alpha1.DatabaseEngineMariaDB, true, true)
	wiki := storesProfile("wiki", gentianov1alpha1.DatabaseEnginePostgreSQL, false, false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(shop, wiki).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	// Installed: provisioning writes down what it is about to make.
	if err := r.recordProvisionedStores(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	recorded, err := r.provisionedStores(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]backup.Provisioned{
		"shop": {DatabaseEngine: gentianov1alpha1.DatabaseEngineMariaDB, Database: "demo_shop", DatabaseUser: "demo_shop",
			Bucket: "demo-shop", CacheUser: "demo-shop"},
		"wiki": {DatabaseEngine: gentianov1alpha1.DatabaseEnginePostgreSQL, Database: "demo_wiki", DatabaseUser: "demo_wiki"},
	}
	if !reflect.DeepEqual(recorded, want) {
		t.Fatalf("recorded = %+v, want %+v", recorded, want)
	}

	// Both uninstalled, their profiles withdrawn, no Job left anywhere, and
	// no Database object either. Provisioning runs again and takes nothing
	// off the record.
	tenant.Spec.Apps = nil
	for _, p := range []client.Object{shop, wiki} {
		if err := c.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.recordProvisionedStores(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	maria, err := r.collectMariaDBApps(ctx, tenant, CollectForDelete)
	if err != nil || !reflect.DeepEqual(maria, []string{"shop"}) {
		t.Errorf("MariaDB apps to destroy = %v, %v; want shop", maria, err)
	}
	buckets, err := r.collectStorageApps(ctx, tenant, CollectForDelete)
	if err != nil || !reflect.DeepEqual(buckets, []string{"shop"}) {
		t.Errorf("buckets to destroy = %v, %v; want shop's", buckets, err)
	}
	redis, _, err := r.collectCacheApps(ctx, tenant, CollectForDelete)
	if err != nil || !reflect.DeepEqual(redis, []string{"shop"}) {
		t.Errorf("cache users to destroy = %v, %v; want shop's", redis, err)
	}
	c2 := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(mustRecord(t, c)).
		WithLists(emptyDatabaseList()).Build()
	r2 := &TenantReconciler{Client: c2, Scheme: scheme}
	pg, err := r2.collectPostgresAppsForDelete(ctx, tenant)
	if err != nil || !reflect.DeepEqual(pg, []string{"wiki"}) {
		t.Errorf("PostgreSQL apps to destroy = %v, %v; want wiki", pg, err)
	}

	// For provisioning nothing of an uninstalled app is collected.
	if apps, err := r.collectStorageApps(ctx, tenant, CollectForProvision); err != nil || len(apps) != 0 {
		t.Errorf("buckets to provision = %v, %v; want none", apps, err)
	}

	// The record goes with the tenant when its data does, and stays when it
	// stays.
	retained := tenant.DeepCopy()
	retained.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyRetain
	if err := r.deleteProvisionedRecord(ctx, retained); err != nil {
		t.Fatal(err)
	}
	if left, _ := r.provisionedStores(ctx, "demo"); len(left) != 2 {
		t.Fatalf("deletionPolicy Retain removed the record: %v", left)
	}
	if err := r.deleteProvisionedRecord(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if left, _ := r.provisionedStores(ctx, "demo"); len(left) != 0 {
		t.Fatalf("the record outlived the tenant: %v", left)
	}
}

func mustRecord(t *testing.T, c client.Client) *corev1.ConfigMap {
	t.Helper()
	record := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), backup.ProvisionedRecordKey("demo"), record); err != nil {
		t.Fatal(err)
	}
	record.ResourceVersion = ""
	return record
}

func emptyDatabaseList() *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(cnpgGroup + "/" + cnpgVersion)
	list.SetKind(cnpgDatabaseKind + "List")
	return list
}

// A record that cannot be read stops the deletion: it is what says which
// stores there are to destroy.
func TestTenantDeleteStopsOnAnUnreadableRecord(t *testing.T) {
	scheme := deleteGapsScheme()
	broken := backup.NewProvisionedRecord("demo")
	broken.Data = map[string]string{"shop": "{not json"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(broken).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete},
	}
	if err := r.deleteStorage(context.Background(), tenant); err == nil {
		t.Fatal("the buckets step went on without knowing which buckets there are")
	}
}
