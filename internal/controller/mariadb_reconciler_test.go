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
"k8s.io/apimachinery/pkg/types"

gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// newMariaDBProfile creates a minimal AppProfile that requires a MariaDB database.
func newMariaDBProfile(name string) *gentianov1alpha1.AppProfile {
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
Engine:            gentianov1alpha1.DatabaseEngineMariaDB,
DatabasePerTenant: true,
},
},
},
}
}

// TestMariaDB_NoMariaDBApps verifies that a Tenant with no apps skips MariaDB
// provisioning and sets MariaDBReady=True with reason NoMariaDBRequired.
func TestMariaDB_NoMariaDBApps(t *testing.T) {
tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "nomaria"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "No Maria Co",
Domain:      "nomaria.example.com",
AdminEmail:  "admin@nomaria.example.com",
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

updated := &gentianov1alpha1.Tenant{}
waitFor(t, 10*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "nomaria"}, updated)
return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
})

var cond *metav1.Condition
for i := range updated.Status.Conditions {
if updated.Status.Conditions[i].Type == "MariaDBReady" {
cond = &updated.Status.Conditions[i]
break
}
}
if cond == nil {
t.Fatal("expected MariaDBReady condition")
}
if cond.Status != metav1.ConditionTrue {
t.Errorf("expected MariaDBReady=True, got %v", cond.Status)
}
if cond.Reason != "NoMariaDBRequired" {
t.Errorf("expected reason NoMariaDBRequired, got %q", cond.Reason)
}

// No setup Job should have been created.
job := &batchv1.Job{}
if err := testClient.Get(context.Background(),
types.NamespacedName{Name: "mariadb-setup-nomaria-anything", Namespace: "platform-kernel"}, job); err == nil {
t.Error("expected no setup Job for Tenant with no MariaDB apps")
}
}

// TestMariaDB_CreatesSetupJob verifies that a Tenant with a MariaDB-requiring app
// creates the setup Job in the kernel namespace with correct labels and container spec.
func TestMariaDB_CreatesSetupJob(t *testing.T) {
profile := newMariaDBProfile("maria-app1")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "mariacreate"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "Maria Create Co",
Domain:      "mariacreate.example.com",
AdminEmail:  "admin@mariacreate.example.com",
Apps:        []gentianov1alpha1.TenantApp{{Profile: "maria-app1"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

job := &batchv1.Job{}
waitFor(t, 10*time.Second, func() bool {
return testClient.Get(context.Background(),
types.NamespacedName{Name: "mariadb-setup-mariacreate-maria-app1", Namespace: "platform-kernel"}, job) == nil
})

if job.Labels["gentianos.io/tenant"] != "mariacreate" {
t.Errorf("expected tenant label 'mariacreate', got %q", job.Labels["gentianos.io/tenant"])
}
if job.Labels["gentianos.io/app"] != "maria-app1" {
t.Errorf("expected app label 'maria-app1', got %q", job.Labels["gentianos.io/app"])
}
if len(job.Spec.Template.Spec.Containers) == 0 {
t.Fatal("expected at least one container in setup Job")
}
container := job.Spec.Template.Spec.Containers[0]
if container.Image != "mariadb:11" {
t.Errorf("unexpected container image %q", container.Image)
}

// DB_NAME and DB_USER must be present as literal env vars.
envMap := make(map[string]string)
for _, e := range container.Env {
if e.Value != "" {
envMap[e.Name] = e.Value
}
}
if envMap["DB_NAME"] == "" {
t.Error("expected DB_NAME env var to be set")
}
if envMap["DB_USER"] == "" {
t.Error("expected DB_USER env var to be set")
}

// Credentials must come from the mariadb-admin Secret (not literal values).
secretEnvs := make(map[string]string)
for _, e := range container.Env {
if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
secretEnvs[e.Name] = e.ValueFrom.SecretKeyRef.Name
}
}
for _, required := range []string{"MYSQL_HOST", "MYSQL_TCP_PORT", "MYSQL_PWD", "MYSQL_ADMIN_USER"} {
if secretEnvs[required] != "mariadb-admin" {
t.Errorf("expected %s sourced from mariadb-admin Secret, got %q", required, secretEnvs[required])
}
}
}

// TestMariaDB_SetsReadyWhenJobsDone verifies that MariaDBReady=True and Phase=Ready
// are set only after all setup Jobs have completed.
func TestMariaDB_SetsReadyWhenJobsDone(t *testing.T) {
profile := newMariaDBProfile("maria-app2")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "mariaready"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "Maria Ready Co",
Domain:      "mariaready.example.com",
AdminEmail:  "admin@mariaready.example.com",
Apps:        []gentianov1alpha1.TenantApp{{Profile: "maria-app2"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

// Phase should be Provisioning while setup Job is pending.
updated := &gentianov1alpha1.Tenant{}
waitFor(t, 10*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mariaready"}, updated)
return updated.Status.Phase == gentianov1alpha1.TenantPhaseProvisioning
})

// Wait for setup Job then mark it complete.
waitFor(t, 10*time.Second, func() bool {
job := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "mariadb-setup-mariaready-maria-app2", Namespace: "platform-kernel"}, job) == nil
})
markJobComplete(t, "mariadb-setup-mariaready-maria-app2", "platform-kernel")

// Phase=Ready and MariaDBReady=True should follow.
waitFor(t, 15*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "mariaready"}, updated)
return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
})

var cond *metav1.Condition
for i := range updated.Status.Conditions {
if updated.Status.Conditions[i].Type == "MariaDBReady" {
cond = &updated.Status.Conditions[i]
break
}
}
if cond == nil || cond.Status != metav1.ConditionTrue {
t.Errorf("expected MariaDBReady=True, got %v", cond)
}
}

// TestMariaDB_DeleteDeletePolicy_CreatesDeleteJob verifies that the delete Job is
// created when DeletionPolicy=Delete and the Tenant is deleted.
func TestMariaDB_DeleteDeletePolicy_CreatesDeleteJob(t *testing.T) {
profile := newMariaDBProfile("maria-app3")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "mariadelete"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName:    "Maria Delete Co",
Domain:         "mariadelete.example.com",
AdminEmail:     "admin@mariadelete.example.com",
DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
Apps:           []gentianov1alpha1.TenantApp{{Profile: "maria-app3"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}

// Wait for the setup Job to be created first.
waitFor(t, 10*time.Second, func() bool {
job := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "mariadb-setup-mariadelete-maria-app3", Namespace: "platform-kernel"}, job) == nil
})

// Delete the tenant.
if err := testClient.Delete(context.Background(), tenant); err != nil {
t.Fatalf("delete tenant: %v", err)
}

// A delete Job should be created in the kernel namespace.
deleteJob := &batchv1.Job{}
waitFor(t, 10*time.Second, func() bool {
return testClient.Get(context.Background(),
types.NamespacedName{Name: "mariadb-delete-mariadelete-maria-app3", Namespace: "platform-kernel"}, deleteJob) == nil
})

if deleteJob.Labels["gentianos.io/tenant"] != "mariadelete" {
t.Errorf("expected tenant label 'mariadelete', got %q", deleteJob.Labels["gentianos.io/tenant"])
}
container := deleteJob.Spec.Template.Spec.Containers[0]
if container.Image != "mariadb:11" {
t.Errorf("unexpected delete Job container image %q", container.Image)
}
}
