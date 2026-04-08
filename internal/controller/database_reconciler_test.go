/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller_test

import (
"context"
"testing"
"time"

batchv1 "k8s.io/api/batch/v1"
metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
"k8s.io/apimachinery/pkg/runtime/schema"
"k8s.io/apimachinery/pkg/types"

gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// newPostgresProfile creates a minimal AppProfile that requires a PostgreSQL database.
func newPostgresProfile(name string) *gentianov1alpha1.AppProfile {
return &gentianov1alpha1.AppProfile{
ObjectMeta: metav1.ObjectMeta{Name: name},
Spec: gentianov1alpha1.AppProfileSpec{
DisplayName: name,
Chart: gentianov1alpha1.ChartRef{
Repository: "https://charts.example.com",
Name:       name,
Version:    "1.0.0",
},
KernelRequirements: &gentianov1alpha1.KernelRequirements{
Database: &gentianov1alpha1.DatabaseRequirement{
Engine:            gentianov1alpha1.DatabaseEnginePostgreSQL,
DatabasePerTenant: true,
},
},
},
}
}

// patchDatabaseCRReady patches the CloudNativePG Database CR status to Ready=True.
func patchDatabaseCRReady(t *testing.T, name, namespace string) {
t.Helper()

dbGVK := schema.GroupVersionKind{
Group:   "postgresql.cnpg.io",
Version: "v1",
Kind:    "Database",
}
db := &unstructured.Unstructured{}
db.SetGroupVersionKind(dbGVK)

if err := testClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, db); err != nil {
t.Fatalf("get Database CR %s: %v", name, err)
}

now := metav1.Now()
conditions := []interface{}{
map[string]interface{}{
"type":               "Ready",
"status":             "True",
"reason":             "DatabaseReconciled",
"message":            "Database is ready",
"lastTransitionTime": now.UTC().Format(time.RFC3339),
},
}
if err := unstructured.SetNestedSlice(db.Object, conditions, "status", "conditions"); err != nil {
t.Fatalf("set Database conditions: %v", err)
}
if err := testClient.Status().Update(context.Background(), db); err != nil {
t.Fatalf("patch Database %s status: %v", name, err)
}
}

// TestDB_NoPostgresApps verifies that a Tenant with no apps does not create any
// Database CRs or role Jobs, and gets DatabaseReady=True with NoDatabaseRequired.
func TestDB_NoPostgresApps(t *testing.T) {
tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "nodb"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "No DB Co",
Domain:      "nodb.example.com",
AdminEmail:  "admin@nodb.example.com",
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

updated := &gentianov1alpha1.Tenant{}
waitFor(t, 10*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "nodb"}, updated)
return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
})

var dbCond *metav1.Condition
for i := range updated.Status.Conditions {
if updated.Status.Conditions[i].Type == "DatabaseReady" {
dbCond = &updated.Status.Conditions[i]
break
}
}
if dbCond == nil {
t.Fatal("expected DatabaseReady condition")
}
if dbCond.Status != metav1.ConditionTrue {
t.Errorf("expected DatabaseReady=True, got %v", dbCond.Status)
}
if dbCond.Reason != "NoDatabaseRequired" {
t.Errorf("expected reason NoDatabaseRequired, got %q", dbCond.Reason)
}
}

// TestDB_CreatesDatabaseCR verifies that a Tenant with an app requiring PostgreSQL
// creates the CloudNativePG Database CR in platform-kernel (after the role Job completes).
func TestDB_CreatesDatabaseCR(t *testing.T) {
	profile := newPostgresProfile("pg-app1")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "dbcreate"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "DB Create Co",
			Domain:      "dbcreate.example.com",
			AdminEmail:  "admin@dbcreate.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "pg-app1"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Step 1: role Job must be created first.
	waitFor(t, 10*time.Second, func() bool {
		job := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "pg-role-dbcreate-pg-app1", Namespace: "platform-kernel"}, job) == nil
	})

	// DB CR must NOT exist before role Job completes.
	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"})
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "db-dbcreate-pg-app1", Namespace: "platform-kernel"}, db); err == nil {
		t.Error("Database CR must not exist before role Job completes")
	}

	// Step 2: mark role Job complete; DB CR should then be created in platform-kernel.
	markJobComplete(t, "pg-role-dbcreate-pg-app1", "platform-kernel")

	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "db-dbcreate-pg-app1", Namespace: "platform-kernel"}, db) == nil
	})

	clusterName, _, _ := unstructured.NestedString(db.Object, "spec", "cluster", "name")
	if clusterName != "postgres" {
		t.Errorf("expected cluster name 'postgres', got %q", clusterName)
	}
	dbSpecName, _, _ := unstructured.NestedString(db.Object, "spec", "name")
	if dbSpecName == "" {
		t.Error("expected spec.name to be set")
	}
	owner, _, _ := unstructured.NestedString(db.Object, "spec", "owner")
	if owner == "" {
		t.Error("expected spec.owner to be set")
	}
}

// TestDB_CreatesDatabaseCRAfterRoleJobCompletes verifies that the role Job is created
// first and the Database CR is only created once the role Job has completed.
func TestDB_CreatesDatabaseCRAfterRoleJobCompletes(t *testing.T) {
	profile := newPostgresProfile("pg-app2")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "rolejob"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Role Job Co",
			Domain:      "rolejob.example.com",
			AdminEmail:  "admin@rolejob.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "pg-app2"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Step 1: role Job must be created immediately (before Database CR).
	roleJob := &batchv1.Job{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "pg-role-rolejob-pg-app2", Namespace: "platform-kernel"}, roleJob) == nil
	})

	// Database CR must NOT exist before role Job completes.
	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"})
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "db-rolejob-pg-app2", Namespace: "platform-kernel"}, db); err == nil {
		t.Error("Database CR must not exist before role Job completes")
	}

	// Mark role Job as complete; Database CR should now be created in platform-kernel.
	markJobComplete(t, "pg-role-rolejob-pg-app2", "platform-kernel")

	waitFor(t, 15*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "db-rolejob-pg-app2", Namespace: "platform-kernel"}, db) == nil
	})

	if roleJob.Labels["gentianos.io/tenant"] != "rolejob" {
		t.Errorf("expected tenant label 'rolejob', got %q", roleJob.Labels["gentianos.io/tenant"])
	}
	if roleJob.Labels["gentianos.io/app"] != "pg-app2" {
		t.Errorf("expected app label 'pg-app2', got %q", roleJob.Labels["gentianos.io/app"])
	}
	if len(roleJob.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container in role Job")
	}
	container := roleJob.Spec.Template.Spec.Containers[0]
	if container.Image != "bitnami/postgresql:16" {
		t.Errorf("unexpected container image %q", container.Image)
	}
}

// TestDB_SetsReadyWhenAllDone verifies that DatabaseReady=True and Phase=Ready are
// set only after the Database CR is ready and the role Job has completed.
func TestDB_SetsReadyWhenAllDone(t *testing.T) {
profile := newPostgresProfile("pg-app3")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "dbready"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "DB Ready Co",
Domain:      "dbready.example.com",
AdminEmail:  "admin@dbready.example.com",
Apps:        []gentianov1alpha1.TenantApp{{Profile: "pg-app3"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

// Phase should be Provisioning (waiting for DB)
updated := &gentianov1alpha1.Tenant{}
waitFor(t, 10*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "dbready"}, updated)
return updated.Status.Phase == gentianov1alpha1.TenantPhaseProvisioning
})

// Step 1: wait for role Job then mark it complete.
	waitFor(t, 10*time.Second, func() bool {
		job := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "pg-role-dbready-pg-app3", Namespace: "platform-kernel"}, job) == nil
	})
	markJobComplete(t, "pg-role-dbready-pg-app3", "platform-kernel")

	// Step 2: wait for Database CR in platform-kernel then mark it ready.
	waitFor(t, 15*time.Second, func() bool {
		db := &unstructured.Unstructured{}
		db.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"})
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "db-dbready-pg-app3", Namespace: "platform-kernel"}, db) == nil
	})
	patchDatabaseCRReady(t, "db-dbready-pg-app3", "platform-kernel")

// Now Phase=Ready and DatabaseReady=True
waitFor(t, 15*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "dbready"}, updated)
return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
})

var dbCond *metav1.Condition
for i := range updated.Status.Conditions {
if updated.Status.Conditions[i].Type == "DatabaseReady" {
dbCond = &updated.Status.Conditions[i]
break
}
}
if dbCond == nil || dbCond.Status != metav1.ConditionTrue {
t.Errorf("expected DatabaseReady=True, got %v", dbCond)
}
}

// TestDB_DeleteDeletePolicy_DeletesDatabaseCR verifies that the Database CR is
// deleted when DeletionPolicy=Delete.
func TestDB_DeleteDeletePolicy_DeletesDatabaseCR(t *testing.T) {
profile := newPostgresProfile("pg-app4")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "dbdelete"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName:     "DB Delete Co",
Domain:          "dbdelete.example.com",
AdminEmail:      "admin@dbdelete.example.com",
DeletionPolicy:  gentianov1alpha1.DeletionPolicyDelete,
Apps:            []gentianov1alpha1.TenantApp{{Profile: "pg-app4"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}

// Step 1: wait for role Job then mark it complete.
waitFor(t, 10*time.Second, func() bool {
job := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "pg-role-dbdelete-pg-app4", Namespace: "platform-kernel"}, job) == nil
})
markJobComplete(t, "pg-role-dbdelete-pg-app4", "platform-kernel")

// Step 2: wait for Database CR to be created in platform-kernel.
waitFor(t, 10*time.Second, func() bool {
db := &unstructured.Unstructured{}
db.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"})
return testClient.Get(context.Background(),
types.NamespacedName{Name: "db-dbdelete-pg-app4", Namespace: "platform-kernel"}, db) == nil
})

// Delete the tenant.
if err := testClient.Delete(context.Background(), tenant); err != nil {
t.Fatalf("delete tenant: %v", err)
}

// Database CR should be deleted from platform-kernel.
waitFor(t, 10*time.Second, func() bool {
db := &unstructured.Unstructured{}
db.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"})
err := testClient.Get(context.Background(),
types.NamespacedName{Name: "db-dbdelete-pg-app4", Namespace: "platform-kernel"}, db)
return err != nil // gone
})
}
