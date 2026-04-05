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
corev1 "k8s.io/api/core/v1"
metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
"k8s.io/apimachinery/pkg/types"

gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// newOIDCProfile creates a minimal AppProfile that requires OIDC.
func newOIDCProfile(name string) *gentianov1alpha1.AppProfile {
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
Identity: &gentianov1alpha1.IdentityRequirement{OIDC: true},
},
},
}
}

// markJobComplete patches a Job's status to Complete so the reconciler sees it as done.
func markJobComplete(t *testing.T, jobName, namespace string) {
t.Helper()
job := &batchv1.Job{}
if err := testClient.Get(context.Background(), types.NamespacedName{Name: jobName, Namespace: namespace}, job); err != nil {
t.Fatalf("get Job %s: %v", jobName, err)
}
now := metav1.Now()
job.Status.StartTime = &now
job.Status.CompletionTime = &now
job.Status.Succeeded = 1
job.Status.Conditions = []batchv1.JobCondition{
{
Type:               batchv1.JobSuccessCriteriaMet,
Status:             corev1.ConditionTrue,
LastProbeTime:      now,
LastTransitionTime: now,
},
{
Type:               batchv1.JobComplete,
Status:             corev1.ConditionTrue,
LastProbeTime:      now,
LastTransitionTime: now,
},
}
if err := testClient.Status().Update(context.Background(), job); err != nil {
t.Fatalf("update Job %s status: %v", jobName, err)
}
}

// TestIdentity_NoOIDCApps verifies that a Tenant with no apps does not create
// any identity Jobs and gets IdentityReady=True with reason NoIdentityRequired.
func TestIdentity_NoOIDCApps(t *testing.T) {
tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "noidc"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "No OIDC Co",
Domain:      "noidc.example.com",
AdminEmail:  "admin@noidc.example.com",
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

updated := &gentianov1alpha1.Tenant{}
waitFor(t, 10*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "noidc"}, updated)
return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
})

var identCond *metav1.Condition
for i := range updated.Status.Conditions {
if updated.Status.Conditions[i].Type == "IdentityReady" {
identCond = &updated.Status.Conditions[i]
break
}
}
if identCond == nil {
t.Fatal("expected IdentityReady condition")
}
if identCond.Status != metav1.ConditionTrue {
t.Errorf("expected IdentityReady=True, got %v", identCond.Status)
}
if identCond.Reason != "NoIdentityRequired" {
t.Errorf("expected reason NoIdentityRequired, got %q", identCond.Reason)
}

// No realm Job should have been created.
job := &batchv1.Job{}
if err := testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-realm-noidc", Namespace: "platform-kernel"}, job); err == nil {
t.Error("expected no realm Job for Tenant with no OIDC apps")
}
}

// TestIdentity_CreatesRealmJob verifies that a Tenant with an OIDC-requiring app
// triggers creation of the Keycloak realm Job in the kernel namespace.
func TestIdentity_CreatesRealmJob(t *testing.T) {
profile := newOIDCProfile("oidc-app1")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "realmtest"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "Realm Test Co",
Domain:      "realmtest.example.com",
AdminEmail:  "admin@realmtest.example.com",
Apps:        []gentianov1alpha1.TenantApp{{Profile: "oidc-app1"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

job := &batchv1.Job{}
waitFor(t, 10*time.Second, func() bool {
return testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-realm-realmtest", Namespace: "platform-kernel"}, job) == nil
})

if job.Labels["gentianos.io/tenant"] != "realmtest" {
t.Errorf("expected tenant label, got %q", job.Labels["gentianos.io/tenant"])
}
if len(job.Spec.Template.Spec.Containers) == 0 {
t.Fatal("expected at least one container in realm Job")
}
container := job.Spec.Template.Spec.Containers[0]
if container.Image != "curlimages/curl:8.7.1" {
t.Errorf("unexpected container image %q", container.Image)
}
if len(container.Env) < 2 {
t.Errorf("expected at least 2 env vars (KEYCLOAK_URL, KEYCLOAK_ADMIN_PASSWORD), got %d", len(container.Env))
}
}

// TestIdentity_CreatesClientJobAfterRealmComplete verifies that the reconciler
// waits for the realm Job to complete before creating the OIDC client Job.
func TestIdentity_CreatesClientJobAfterRealmComplete(t *testing.T) {
profile := newOIDCProfile("oidc-app2")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "clienttest"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "Client Test Co",
Domain:      "clienttest.example.com",
AdminEmail:  "admin@clienttest.example.com",
Apps:        []gentianov1alpha1.TenantApp{{Profile: "oidc-app2"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

// Wait for realm Job, then mark it complete.
waitFor(t, 10*time.Second, func() bool {
j := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-realm-clienttest", Namespace: "platform-kernel"}, j) == nil
})
markJobComplete(t, "keycloak-realm-clienttest", "platform-kernel")

// Client Job should be created after realm is complete.
clientJob := &batchv1.Job{}
waitFor(t, 15*time.Second, func() bool {
return testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-client-clienttest-oidc-app2", Namespace: "platform-kernel"}, clientJob) == nil
})

if clientJob.Labels["gentianos.io/app"] != "oidc-app2" {
t.Errorf("expected app label oidc-app2, got %q", clientJob.Labels["gentianos.io/app"])
}
if clientJob.Labels["gentianos.io/tenant"] != "clienttest" {
t.Errorf("expected tenant label clienttest, got %q", clientJob.Labels["gentianos.io/tenant"])
}
}

// TestIdentity_SetsReadyWhenAllJobsDone verifies that IdentityReady=True and
// Phase=Ready are set only after both the realm and all client Jobs have completed.
func TestIdentity_SetsReadyWhenAllJobsDone(t *testing.T) {
profile := newOIDCProfile("oidc-app3")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "allready"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName: "All Ready Co",
Domain:      "allready.example.com",
AdminEmail:  "admin@allready.example.com",
Apps:        []gentianov1alpha1.TenantApp{{Profile: "oidc-app3"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

// Mark realm Job complete.
waitFor(t, 10*time.Second, func() bool {
j := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-realm-allready", Namespace: "platform-kernel"}, j) == nil
})
markJobComplete(t, "keycloak-realm-allready", "platform-kernel")

// Wait for client Job, then mark it complete.
waitFor(t, 15*time.Second, func() bool {
j := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-client-allready-oidc-app3", Namespace: "platform-kernel"}, j) == nil
})
markJobComplete(t, "keycloak-client-allready-oidc-app3", "platform-kernel")

// Wait for IdentityReady=True and Phase=Ready.
updated := &gentianov1alpha1.Tenant{}
waitFor(t, 15*time.Second, func() bool {
_ = testClient.Get(context.Background(), types.NamespacedName{Name: "allready"}, updated)
for _, c := range updated.Status.Conditions {
if c.Type == "IdentityReady" && c.Status == metav1.ConditionTrue {
return true
}
}
return false
})

if updated.Status.Phase != gentianov1alpha1.TenantPhaseReady {
t.Errorf("expected Phase=Ready, got %v", updated.Status.Phase)
}
var identCond *metav1.Condition
for i := range updated.Status.Conditions {
if updated.Status.Conditions[i].Type == "IdentityReady" {
identCond = &updated.Status.Conditions[i]
break
}
}
if identCond == nil {
t.Fatal("IdentityReady condition not found")
}
if identCond.Reason != "Provisioned" {
t.Errorf("expected reason Provisioned, got %q", identCond.Reason)
}
}

// TestIdentity_DeleteDeletePolicy_CreatesCleanupJob verifies that deleting a Tenant
// with DeletionPolicy=Delete creates a realm-deletion Job in the kernel namespace.
func TestIdentity_DeleteDeletePolicy_CreatesCleanupJob(t *testing.T) {
profile := newOIDCProfile("oidc-app4")
if err := testClient.Create(context.Background(), profile); err != nil {
t.Fatalf("create AppProfile: %v", err)
}
t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

tenant := &gentianov1alpha1.Tenant{
ObjectMeta: metav1.ObjectMeta{Name: "identdelete"},
Spec: gentianov1alpha1.TenantSpec{
DisplayName:    "Identity Delete Co",
Domain:         "identdelete.example.com",
AdminEmail:     "admin@identdelete.example.com",
DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
Apps:           []gentianov1alpha1.TenantApp{{Profile: "oidc-app4"}},
},
}
if err := testClient.Create(context.Background(), tenant); err != nil {
t.Fatalf("create tenant: %v", err)
}

// Wait until the realm Job is created (tenant has been reconciled at least once).
waitFor(t, 10*time.Second, func() bool {
j := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-realm-identdelete", Namespace: "platform-kernel"}, j) == nil
})

// Now delete the tenant.
if err := testClient.Delete(context.Background(), tenant); err != nil {
t.Fatalf("delete tenant: %v", err)
}

// Expect a realm-deletion cleanup Job.
waitFor(t, 10*time.Second, func() bool {
j := &batchv1.Job{}
return testClient.Get(context.Background(),
types.NamespacedName{Name: "keycloak-realm-delete-identdelete", Namespace: "platform-kernel"}, j) == nil
})
}
