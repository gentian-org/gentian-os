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

func ptrBool(v bool) *bool { return &v }

func clusterPolicy(spec gentianov1alpha1.BackupPolicySpec) *gentianov1alpha1.BackupPolicy {
	spec.Scope = "cluster"
	return &gentianov1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       spec,
	}
}

func tenantPolicy(spec gentianov1alpha1.BackupPolicySpec) *gentianov1alpha1.BackupPolicy {
	spec.Scope, spec.Tenant = "tenant", "demo"
	return &gentianov1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       spec,
	}
}

// No policy at all is the state of every cluster until someone sets one, and
// it must resolve to the platform's own storage rather than to an error.
func TestResolveEffectiveWithNoPolicies(t *testing.T) {
	eff, err := ResolveEffective(policyTenant(), nil, nil)
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if !eff.PlatformStorage() {
		t.Errorf("endpoint = %q, want the platform's own storage", eff.Endpoint)
	}
	if eff.Bucket != "demo-gentian-backup" {
		t.Errorf("bucket = %q, want the per-tenant default", eff.Bucket)
	}
	if eff.CredentialName != "" {
		t.Errorf("platform storage asked for credential %q; it needs none", eff.CredentialName)
	}
}

// A tenant policy with no cluster policy is ordinary on a fresh cluster.
// Reaching through the absent cluster policy to ask whether overrides are
// allowed panicked the reconciler on exactly this input.
func TestResolveEffectiveOverrideWithoutClusterPolicy(t *testing.T) {
	eff, err := ResolveEffective(policyTenant(), nil,
		tenantPolicy(gentianov1alpha1.BackupPolicySpec{Schedule: "0 3 * * *"}))
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.Schedule != "0 3 * * *" {
		t.Errorf("schedule = %q, want the tenant's", eff.Schedule)
	}
}

// The cluster sets the default; the tenant inherits what it does not state.
func TestTenantInheritsClusterDefaults(t *testing.T) {
	cluster := clusterPolicy(gentianov1alpha1.BackupPolicySpec{
		Destination: &gentianov1alpha1.BackupDestination{
			Endpoint: "https://sos-ch-gva-2.exo.io",
			Bucket:   "platform-bundles",
			Region:   "ch-gva-2",
		},
		Schedule:  "0 3 * * *",
		Retention: &gentianov1alpha1.BackupRetention{KeepLast: 7},
	})
	// States only the schedule: the destination must still be the cluster's.
	override := tenantPolicy(gentianov1alpha1.BackupPolicySpec{Schedule: "30 1 * * *"})

	eff, err := ResolveEffective(policyTenant(), cluster, override)
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.Endpoint != "https://sos-ch-gva-2.exo.io" || eff.Bucket != "platform-bundles" {
		t.Errorf("destination = %s/%s, want the cluster's", eff.Endpoint, eff.Bucket)
	}
	if eff.Region != "ch-gva-2" {
		t.Errorf("region = %q, want the cluster's", eff.Region)
	}
	if eff.CredentialName != "backup-destination" {
		t.Errorf("credential = %q, want the cluster's requirement", eff.CredentialName)
	}
	if eff.Schedule != "30 1 * * *" {
		t.Errorf("schedule = %q, want the tenant's", eff.Schedule)
	}
	if eff.Retention.KeepLast != 7 {
		t.Errorf("keepLast = %d, want the inherited 7", eff.Retention.KeepLast)
	}
	if eff.Overridden {
		t.Error("stating only a schedule must not mark the destination overridden")
	}
}

// A tenant's credential is derived from its own scope, which is what puts it
// under the tenant's OpenBao subtree — a tenant admin cannot reach cluster
// paths, so it cannot read or overwrite the platform's own keys.
func TestTenantDestinationDerivesItsOwnCredential(t *testing.T) {
	cluster := clusterPolicy(gentianov1alpha1.BackupPolicySpec{
		Destination: &gentianov1alpha1.BackupDestination{Endpoint: "https://platform.example.org"},
	})
	override := tenantPolicy(gentianov1alpha1.BackupPolicySpec{
		Destination: &gentianov1alpha1.BackupDestination{
			Endpoint: "https://tenant.example.org",
			Bucket:   "my-own-bundles",
		},
	})

	eff, err := ResolveEffective(policyTenant(), cluster, override)
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.CredentialName != "backup-destination-demo" {
		t.Fatalf("credential = %q, want the tenant's own", eff.CredentialName)
	}
	if got := DestinationVaultPath("tenant", "demo"); got != "gentian-os/tenants/demo/backup/destination" {
		t.Errorf("tenant vault path = %q, outside the tenant's subtree", got)
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

// Renaming the bucket within the platform's own storage needs no credential:
// the platform already has keys for its own MinIO.
func TestBucketOnlyChangeNeedsNoCredential(t *testing.T) {
	eff, err := ResolveEffective(policyTenant(),
		clusterPolicy(gentianov1alpha1.BackupPolicySpec{
			Destination: &gentianov1alpha1.BackupDestination{Bucket: "all-bundles"},
		}), nil)
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.Bucket != "all-bundles" {
		t.Errorf("bucket = %q", eff.Bucket)
	}
	if eff.CredentialName != "" || !eff.PlatformStorage() {
		t.Errorf("a bucket rename asked for credential %q at endpoint %q",
			eff.CredentialName, eff.Endpoint)
	}
}

// A cluster that forbids overrides must refuse them, not ignore them: an admin
// who sets a destination and sees bundles go elsewhere has been misled.
func TestForbiddenOverrideIsRefusedNotIgnored(t *testing.T) {
	cluster := clusterPolicy(gentianov1alpha1.BackupPolicySpec{
		AllowTenantOverride: ptrBool(false),
	})
	override := tenantPolicy(gentianov1alpha1.BackupPolicySpec{
		Destination: &gentianov1alpha1.BackupDestination{Endpoint: "https://elsewhere.example.org"},
	})
	_, err := ResolveEffective(policyTenant(), cluster, override)
	if err == nil {
		t.Fatal("a forbidden override was silently accepted")
	}
	if !strings.Contains(err.Error(), "allowTenantOverride") {
		t.Errorf("error does not name the setting that refused it: %v", err)
	}
}

// Suspending is distinct from inheriting: without it a tenant could not opt
// out of a cluster-wide schedule, because "" already means "not stated".
func TestSuspendScheduleOptsOutOfTheClusterDefault(t *testing.T) {
	cluster := clusterPolicy(gentianov1alpha1.BackupPolicySpec{Schedule: "0 3 * * *"})
	override := tenantPolicy(gentianov1alpha1.BackupPolicySpec{SuspendSchedule: true})

	eff, err := ResolveEffective(policyTenant(), cluster, override)
	if err != nil {
		t.Fatalf("ResolveEffective: %v", err)
	}
	if eff.Schedule != "" {
		t.Errorf("schedule = %q, want none after suspending", eff.Schedule)
	}
}

// The three targets a manual backup can choose, and what each does to the
// credential — which is the part that decides whether keys are copied.
func TestExportDestinationChoosesTargetAndCredential(t *testing.T) {
	tenant := policyTenant()
	// What the policy resolved to: the workspace's own external destination,
	// authenticated with the credential the schedule uses.
	policy := Effective{
		Endpoint:         "https://sos-ch-dk-2.exo.io",
		Region:           "ch-dk-2",
		Bucket:           "nightly",
		CredentialName:   "backup-destination-demo",
		CredentialSecret: "backup-destination-demo",
	}

	t.Run("existing: the policy's answer, untouched", func(t *testing.T) {
		got := ApplyExportDestination(policy, nil, tenant, "manual-1")
		if got != policy {
			t.Errorf("an unstated destination changed the policy: %+v", got)
		}
		if got.Overridden {
			t.Error("Overridden set for a destination that overrode nothing")
		}
	})

	t.Run("local: platform storage, and nothing addressing elsewhere", func(t *testing.T) {
		got := ApplyExportDestination(policy,
			&gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationPlatform},
			tenant, "manual-2")
		if !got.PlatformStorage() {
			t.Errorf("endpoint = %q, want empty so the platform's own storage is used", got.Endpoint)
		}
		// Half a destination — an external credential with no endpoint, or the
		// reverse — addresses no storage that exists.
		if got.Region != "" || got.CredentialName != "" || got.CredentialSecret != "" {
			t.Errorf("something still addresses the external destination: %+v", got)
		}
		if got.Bucket != BackupBucket(tenant) {
			t.Errorf("bucket = %q, want the tenant's own %q", got.Bucket, BackupBucket(tenant))
		}
	})

	t.Run("S3 with the managed credential: no keys are copied", func(t *testing.T) {
		got := ApplyExportDestination(policy, &gentianov1alpha1.ExportDestination{
			Mode:             gentianov1alpha1.ExportDestinationCustom,
			CredentialSource: gentianov1alpha1.ExportCredentialManaged,
			Endpoint:         "https://sos-ch-gva-2.exo.io",
			Bucket:           "one-off",
			Region:           "ch-gva-2",
		}, tenant, "manual-3")

		if got.Endpoint != "https://sos-ch-gva-2.exo.io" || got.Bucket != "one-off" {
			t.Errorf("endpoint/bucket not taken from the export: %+v", got)
		}
		// The whole point of managed: the Secret ESO already materialised is
		// used where it stands. A staged copy here would be a second copy of a
		// live credential for no gain.
		if got.CredentialSecret != policy.CredentialSecret {
			t.Errorf("CredentialSecret = %q, want the workspace's own %q",
				got.CredentialSecret, policy.CredentialSecret)
		}
		if got.CredentialSecret == ExportCredentialSecretName("manual-3") {
			t.Error("managed source staged a copy; nothing should have been copied")
		}
	})

	t.Run("S3 with transient keys: staged under the export's own name", func(t *testing.T) {
		got := ApplyExportDestination(policy, &gentianov1alpha1.ExportDestination{
			Mode:                gentianov1alpha1.ExportDestinationCustom,
			CredentialSource:    gentianov1alpha1.ExportCredentialTransient,
			Endpoint:            "https://s3.example.org",
			CredentialSecretRef: "someone-elses-secret",
		}, tenant, "manual-4")

		want := ExportCredentialSecretName("manual-4")
		if got.CredentialSecret != want {
			t.Errorf("CredentialSecret = %q, want %q", got.CredentialSecret, want)
		}
		// Derived from the export, never from the reference: a tenant that
		// could name the staged Secret could aim a capture Job at any Secret in
		// the kernel namespace.
		if strings.Contains(got.CredentialSecret, "someone-elses-secret") {
			t.Error("the staged name came from the requester's reference")
		}
		if got.CredentialName != "" {
			t.Errorf("CredentialName = %q, want empty: one-off keys are not a "+
				"standing requirement anyone administers", got.CredentialName)
		}
	})
}
