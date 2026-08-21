/*
Copyright 2026 Gentian Organization.

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

package backup

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func policyTenant() *gentianov1alpha1.Tenant {
	return &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Isolation: &gentianov1alpha1.TenantIsolation{S3Prefix: "demo-"},
		},
	}
}

func ptrInt32(v int32) *int32 { return &v }
func ptrBool(v bool) *bool    { return &v }

// No policy at all is the state of every cluster until someone sets one, and
// it must resolve to the platform's own storage rather than to an error.
func TestResolveEffectiveWithNoPolicies(t *testing.T) {
	eff, err := ResolveEffective(policyTenant(), nil, nil, "platform-kernel")
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if !eff.PlatformStorage() {
		t.Errorf("endpoint = %q, want the platform's own storage", eff.Endpoint)
	}
	if eff.Bucket != "demo-gentian-backup" {
		t.Errorf("bucket = %q, want the per-tenant default", eff.Bucket)
	}
	if eff.Schedule != "" || eff.Overridden {
		t.Errorf("unset policy produced schedule %q overridden=%v", eff.Schedule, eff.Overridden)
	}
}

// A tenant override with no cluster policy is ordinary on a fresh cluster.
// Reaching through the absent cluster policy to ask whether overrides are
// allowed panicked the reconciler on exactly this input.
func TestResolveEffectiveOverrideWithoutClusterPolicy(t *testing.T) {
	override := &gentianov1alpha1.TenantBackupPolicy{
		Spec: gentianov1alpha1.TenantBackupPolicySpec{Schedule: "0 3 * * *"},
	}
	eff, err := ResolveEffective(policyTenant(), nil, override, "platform-kernel")
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.Schedule != "0 3 * * *" {
		t.Errorf("schedule = %q, want the tenant's", eff.Schedule)
	}
}

// The cluster sets the default; the tenant inherits what it does not state.
func TestTenantInheritsClusterDefaults(t *testing.T) {
	cluster := &gentianov1alpha1.BackupPolicy{
		Spec: gentianov1alpha1.BackupPolicySpec{
			Destination: &gentianov1alpha1.BackupDestination{
				Endpoint:             "https://s3.example.org",
				Bucket:               "platform-bundles",
				CredentialsSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "cluster-s3"},
			},
			Schedule: "0 3 * * *",
			KeepLast: 7,
		},
	}
	// Overrides only the schedule: the destination must still be the cluster's.
	override := &gentianov1alpha1.TenantBackupPolicy{
		Spec: gentianov1alpha1.TenantBackupPolicySpec{Schedule: "30 1 * * *"},
	}

	eff, err := ResolveEffective(policyTenant(), cluster, override, "platform-kernel")
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.Endpoint != "https://s3.example.org" || eff.Bucket != "platform-bundles" {
		t.Errorf("destination = %s/%s, want the cluster's", eff.Endpoint, eff.Bucket)
	}
	if eff.CredentialsNamespace != "platform-kernel" {
		t.Errorf("cluster credentials read from %q, want the kernel namespace", eff.CredentialsNamespace)
	}
	if eff.Schedule != "30 1 * * *" {
		t.Errorf("schedule = %q, want the tenant's override", eff.Schedule)
	}
	if eff.KeepLast != 7 {
		t.Errorf("keepLast = %d, want the inherited 7", eff.KeepLast)
	}
	if eff.Overridden {
		t.Error("overriding only the schedule must not mark the destination overridden")
	}
}

// A tenant's credentials come from the tenant's own namespace. Reading them
// from anywhere else would let a tenant name a Secret it does not own.
func TestTenantDestinationReadsCredentialsFromItsOwnNamespace(t *testing.T) {
	cluster := &gentianov1alpha1.BackupPolicy{
		Spec: gentianov1alpha1.BackupPolicySpec{
			Destination: &gentianov1alpha1.BackupDestination{
				Endpoint:             "https://platform.example.org",
				CredentialsSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "cluster-s3"},
			},
		},
	}
	override := &gentianov1alpha1.TenantBackupPolicy{
		Spec: gentianov1alpha1.TenantBackupPolicySpec{
			Destination: &gentianov1alpha1.BackupDestination{
				Endpoint:             "https://tenant.example.org",
				Bucket:               "my-own-bundles",
				CredentialsSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "my-s3"},
			},
		},
	}

	eff, err := ResolveEffective(policyTenant(), cluster, override, "platform-kernel")
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.CredentialsNamespace != "tenant-demo" {
		t.Fatalf("tenant credentials read from %q, want tenant-demo", eff.CredentialsNamespace)
	}
	if eff.CredentialsSecret != "my-s3" {
		t.Errorf("credentials secret = %q, want the tenant's", eff.CredentialsSecret)
	}
	// The cluster's endpoint must not survive alongside the tenant's: half of
	// each addresses no storage that exists.
	if eff.Endpoint != "https://tenant.example.org" {
		t.Errorf("endpoint = %q, want the tenant's", eff.Endpoint)
	}
	if !eff.Overridden {
		t.Error("a tenant destination must be marked as overridden")
	}
}

// An endpoint without credentials is refused rather than falling back to the
// platform's, which authenticate to the platform's MinIO and nothing else.
func TestEndpointWithoutCredentialsIsRefused(t *testing.T) {
	cluster := &gentianov1alpha1.BackupPolicy{
		Spec: gentianov1alpha1.BackupPolicySpec{
			Destination: &gentianov1alpha1.BackupDestination{Endpoint: "https://s3.example.org"},
		},
	}
	if _, err := ResolveEffective(policyTenant(), cluster, nil, "platform-kernel"); err == nil {
		t.Fatal("an endpoint with no credentials was accepted")
	} else if !strings.Contains(err.Error(), "credentialsSecretRef") {
		t.Errorf("error does not name the missing field: %v", err)
	}
}

// A cluster that forbids overrides must refuse them, not ignore them: an admin
// who sets a destination and sees bundles go elsewhere has been misled.
func TestForbiddenOverrideIsRefusedNotIgnored(t *testing.T) {
	cluster := &gentianov1alpha1.BackupPolicy{
		Spec: gentianov1alpha1.BackupPolicySpec{AllowTenantOverride: ptrBool(false)},
	}
	override := &gentianov1alpha1.TenantBackupPolicy{
		Spec: gentianov1alpha1.TenantBackupPolicySpec{
			Destination: &gentianov1alpha1.BackupDestination{
				Endpoint:             "https://elsewhere.example.org",
				CredentialsSecretRef: &gentianov1alpha1.SecretKeyRef{Name: "mine"},
			},
		},
	}
	if _, err := ResolveEffective(policyTenant(), cluster, override, "platform-kernel"); err == nil {
		t.Fatal("a forbidden override was silently accepted")
	}
}

// Suspending is distinct from inheriting: without it a tenant could not opt
// out of a cluster-wide schedule, because "" already means "not stated".
func TestSuspendScheduleOptsOutOfTheClusterDefault(t *testing.T) {
	cluster := &gentianov1alpha1.BackupPolicy{
		Spec: gentianov1alpha1.BackupPolicySpec{Schedule: "0 3 * * *", KeepLast: 7},
	}
	override := &gentianov1alpha1.TenantBackupPolicy{
		Spec: gentianov1alpha1.TenantBackupPolicySpec{
			SuspendSchedule: true,
			KeepLast:        ptrInt32(0),
		},
	}
	eff, err := ResolveEffective(policyTenant(), cluster, override, "platform-kernel")
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.Schedule != "" {
		t.Errorf("schedule = %q, want none after suspending", eff.Schedule)
	}
	if eff.KeepLast != 0 {
		t.Errorf("keepLast = %d, want the explicit 0", eff.KeepLast)
	}
}
