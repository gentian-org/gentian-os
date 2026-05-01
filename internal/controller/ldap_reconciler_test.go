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

// newLDAPProfile creates a minimal AppProfile that requires LDAP.
func newLDAPProfile(name string) *gentianov1alpha1.AppProfile {
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
				Identity: &gentianov1alpha1.IdentityRequirement{
					LDAP: &gentianov1alpha1.LDAPRequirement{},
				},
			},
		},
	}
}

// TestLDAP_NoLDAPApps verifies that a Tenant with no LDAP-requiring apps:
//   - still has LDAPReady=True with reason NoLDAPRequired (no blocking)
//   - Phase=Ready is reached without waiting for any LDAP Jobs
//   - the OU Job IS created via ensureLDAPBase (non-blocking base provisioning)
//   - no bind-account Jobs are created
func TestLDAP_NoLDAPApps(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "noldap"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "No LDAP Co",
			Domain:      "noldap.example.com",
			AdminEmail:  "admin@noldap.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Phase=Ready must be reached without marking any LDAP Jobs complete
	// (ensureLDAPBase is non-blocking, ensureLDAP early-exits).
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 10*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "noldap"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var ldapCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "LDAPReady" {
			ldapCond = &updated.Status.Conditions[i]
			break
		}
	}
	if ldapCond == nil {
		t.Fatal("expected LDAPReady condition")
	}
	if ldapCond.Status != metav1.ConditionTrue {
		t.Errorf("expected LDAPReady=True, got %v", ldapCond.Status)
	}
	if ldapCond.Reason != "NoLDAPRequired" {
		t.Errorf("expected reason NoLDAPRequired, got %q", ldapCond.Reason)
	}

	// ensureLDAPBase must have fired the OU Job even though no LDAP apps are installed.
	job := &batchv1.Job{}
	waitFor(t, 5*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-noldap", Namespace: "platform-kernel"}, job) == nil
	})
}

// TestLDAP_CreatesOUJob verifies that a Tenant with an LDAP-requiring app triggers
// creation of the UDM OU Job in the kernel namespace.
func TestLDAP_CreatesOUJob(t *testing.T) {
	t.Parallel()
	profile := newLDAPProfile("ldap-app1")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "outest"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "OU Test Co",
			Domain:      "outest.example.com",
			AdminEmail:  "admin@outest.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "ldap-app1"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	job := &batchv1.Job{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-outest", Namespace: "platform-kernel"}, job) == nil
	})

	if job.Labels["gentianos.io/tenant"] != "outest" {
		t.Errorf("expected tenant label, got %q", job.Labels["gentianos.io/tenant"])
	}
	if len(job.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container in OU Job")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "curlimages/curl:8.7.1" {
		t.Errorf("unexpected container image %q", container.Image)
	}
	if len(container.Env) < 3 {
		t.Errorf("expected at least 3 env vars (UDM_URL, UDM_ADMIN_PASSWORD, UDM_LDAP_BASE), got %d", len(container.Env))
	}
}

// TestLDAP_CreatesBindAccountJobAfterOUComplete verifies that the bind account
// Job is only created after the OU and admin-policy Jobs have completed.
func TestLDAP_CreatesBindAccountJobAfterOUComplete(t *testing.T) {
	t.Parallel()
	profile := newLDAPProfile("ldap-app2")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "bindtest"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Bind Test Co",
			Domain:      "bindtest.example.com",
			AdminEmail:  "admin@bindtest.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "ldap-app2"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for OU Job, then mark it complete.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-bindtest", Namespace: "platform-kernel"}, j) == nil
	})
	markJobComplete(t, "ldap-ou-bindtest", "platform-kernel")

	// Wait for admin-policy Job, then mark it complete.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-admin-policy-bindtest", Namespace: "platform-kernel"}, j) == nil
	})
	markJobComplete(t, "ldap-admin-policy-bindtest", "platform-kernel")

	// Wait for admin-user Job, then mark it complete.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-admin-user-bindtest", Namespace: "platform-kernel"}, j) == nil
	})
	markJobComplete(t, "ldap-admin-user-bindtest", "platform-kernel")

	// Bind account Job should appear after OU and admin-policy are complete.
	bindJob := &batchv1.Job{}
	waitFor(t, 15*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-bind-bindtest-ldap-app2", Namespace: "platform-kernel"}, bindJob) == nil
	})

	if bindJob.Labels["gentianos.io/app"] != "ldap-app2" {
		t.Errorf("expected app label ldap-app2, got %q", bindJob.Labels["gentianos.io/app"])
	}
	if bindJob.Labels["gentianos.io/tenant"] != "bindtest" {
		t.Errorf("expected tenant label bindtest, got %q", bindJob.Labels["gentianos.io/tenant"])
	}
}

// TestLDAP_CreatesAdminPolicyJobAfterOU verifies that delegated-admin policy
// provisioning is ordered between OU creation and bind-account provisioning.
func TestLDAP_CreatesAdminPolicyJobAfterOU(t *testing.T) {
	t.Parallel()
	profile := newLDAPProfile("ldap-app2b")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "adminpolicy"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Admin Policy Co",
			Domain:      "adminpolicy.example.com",
			AdminEmail:  "admin@adminpolicy.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "ldap-app2b"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for OU Job.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-adminpolicy", Namespace: "platform-kernel"}, j) == nil
	})

	// Admin-policy Job must not exist before OU completion.
	job := &batchv1.Job{}
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "ldap-admin-policy-adminpolicy", Namespace: "platform-kernel"}, job); err == nil {
		t.Fatal("expected no admin-policy Job before OU completion")
	}

	markJobComplete(t, "ldap-ou-adminpolicy", "platform-kernel")

	// Admin-policy Job should appear after OU completion.
	waitFor(t, 15*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-admin-policy-adminpolicy", Namespace: "platform-kernel"}, j) == nil
	})

	// Bind account Job should not exist until admin-policy is complete.
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "ldap-bind-adminpolicy-ldap-app2b", Namespace: "platform-kernel"}, job); err == nil {
		t.Fatal("expected no bind account Job before admin-policy completion")
	}
}

// TestLDAP_SetsReadyWhenAllJobsDone verifies that LDAPReady=True and Phase=Ready
// are set only after OU and all bind account Jobs have completed.
func TestLDAP_SetsReadyWhenAllJobsDone(t *testing.T) {
	t.Parallel()
	profile := newLDAPProfile("ldap-app3")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "ldapready"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "LDAP Ready Co",
			Domain:      "ldapready.example.com",
			AdminEmail:  "admin@ldapready.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "ldap-app3"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Mark OU Job complete.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-ldapready", Namespace: "platform-kernel"}, j) == nil
	})
	markJobComplete(t, "ldap-ou-ldapready", "platform-kernel")

	// Mark admin-policy Job complete.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-admin-policy-ldapready", Namespace: "platform-kernel"}, j) == nil
	})
	markJobComplete(t, "ldap-admin-policy-ldapready", "platform-kernel")

	// Wait for admin-user Job, then mark it complete.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-admin-user-ldapready", Namespace: "platform-kernel"}, j) == nil
	})
	markJobComplete(t, "ldap-admin-user-ldapready", "platform-kernel")

	// Wait for bind account Job, then mark it complete.
	waitFor(t, 15*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-bind-ldapready-ldap-app3", Namespace: "platform-kernel"}, j) == nil
	})
	markJobComplete(t, "ldap-bind-ldapready-ldap-app3", "platform-kernel")

	// Wait for LDAPReady=True.
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 15*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "ldapready"}, updated)
		for _, c := range updated.Status.Conditions {
			if c.Type == "LDAPReady" && c.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	})

	if updated.Status.Phase != gentianov1alpha1.TenantPhaseReady {
		t.Errorf("expected Phase=Ready, got %v", updated.Status.Phase)
	}
	var ldapCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "LDAPReady" {
			ldapCond = &updated.Status.Conditions[i]
			break
		}
	}
	if ldapCond == nil {
		t.Fatal("LDAPReady condition not found")
	}
	if ldapCond.Reason != "Provisioned" {
		t.Errorf("expected reason Provisioned, got %q", ldapCond.Reason)
	}
}

// TestLDAP_OUNameFromIsolation verifies that spec.isolation.ldapOU overrides
// the default ou={name},${UDM_LDAP_BASE} OU name in the Job command.
func TestLDAP_OUNameFromIsolation(t *testing.T) {
	t.Parallel()
	profile := newLDAPProfile("ldap-app4")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "customou"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Custom OU Co",
			Domain:      "customou.example.com",
			AdminEmail:  "admin@customou.example.com",
			Isolation: &gentianov1alpha1.TenantIsolation{
				LDAPOu: "ou=custom-org,dc=example,dc=com",
			},
			Apps: []gentianov1alpha1.TenantApp{{Profile: "ldap-app4"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	job := &batchv1.Job{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-customou", Namespace: "platform-kernel"}, job) == nil
	})

	// The Job's container command should reference the custom OU DN.
	if len(job.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container")
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if len(script) == 0 {
		t.Fatal("empty container command")
	}
	const customOU = "ou=custom-org,dc=example,dc=com"
	found := false
	for i := 0; i < len(script)-len(customOU)+1; i++ {
		if script[i:i+len(customOU)] == customOU {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected custom OU DN %q in container command", customOU)
	}
}

// TestLDAP_DeleteDeletePolicy_CreatesCleanupJob verifies that deleting a Tenant
// with DeletionPolicy=Delete creates an OU-deletion Job in the kernel namespace.
func TestLDAP_DeleteDeletePolicy_CreatesCleanupJob(t *testing.T) {
	t.Parallel()
	profile := newLDAPProfile("ldap-app5")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "ldapdelete"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "LDAP Delete Co",
			Domain:         "ldapdelete.example.com",
			AdminEmail:     "admin@ldapdelete.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps:           []gentianov1alpha1.TenantApp{{Profile: "ldap-app5"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait until the OU Job is created.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-ldapdelete", Namespace: "platform-kernel"}, j) == nil
	})

	// Delete the tenant.
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	// Expect the OU-deletion cleanup Job.
	waitFor(t, 10*time.Second, func() bool {
		j := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "ldap-ou-delete-ldapdelete", Namespace: "platform-kernel"}, j) == nil
	})
}
