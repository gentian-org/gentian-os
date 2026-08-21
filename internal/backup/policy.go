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
	"fmt"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Effective is what a tenant's backups actually do, after the cluster policy
// and the tenant's own override have been merged.
//
// Resolved in one place and published in status rather than recomputed at each
// use: "where do my backups go" must have exactly one answer, and an admin
// should be able to read it rather than derive it.
type Effective struct {
	// Endpoint is empty for the platform's own MinIO, which is the case the
	// capture Jobs already handle with the kernel's minio-admin credentials.
	Endpoint string
	Bucket   string
	Region   string

	// CredentialsSecret names the Secret holding accessKey and secretKey, and
	// Namespace is where it lives — the kernel namespace for a cluster policy,
	// the tenant's own for an override. Empty means the platform's MinIO.
	CredentialsSecret    string
	CredentialsNamespace string

	// Schedule is a cron expression in UTC; empty means no scheduled backups.
	Schedule string
	KeepLast int32

	// Overridden records that the tenant, not the cluster, chose the
	// destination. The console shows it, and it is the difference between "the
	// platform can help you restore this" and "only you can".
	Overridden bool
}

// PlatformStorage reports whether bundles go to the platform's own MinIO.
func (e Effective) PlatformStorage() bool { return e.Endpoint == "" }

// ResolveEffective merges the cluster policy with a tenant's override.
//
// The tenant's override is refused rather than silently ignored when the
// cluster forbids it: an admin who set a destination and sees backups going
// somewhere else has been misled, which is worse than being told no.
func ResolveEffective(
	tenant *gentianov1alpha1.Tenant,
	cluster *gentianov1alpha1.BackupPolicy,
	override *gentianov1alpha1.TenantBackupPolicy,
	kernelNamespace string,
) (Effective, error) {
	eff := Effective{Bucket: BackupBucket(tenant)}

	if cluster != nil {
		if d := cluster.Spec.Destination; d.IsSet() {
			if err := d.Validate(); err != nil {
				return eff, fmt.Errorf("cluster backup policy: %w", err)
			}
			eff.Endpoint, eff.Region = d.Endpoint, d.Region
			if d.Bucket != "" {
				eff.Bucket = d.Bucket
			}
			if d.CredentialsSecretRef != nil {
				eff.CredentialsSecret = d.CredentialsSecretRef.Name
				eff.CredentialsNamespace = kernelNamespace
			}
		}
		eff.Schedule = cluster.Spec.Schedule
		eff.KeepLast = cluster.Spec.KeepLast
	}

	if override == nil {
		return eff, nil
	}
	// Guarded rather than reaching through cluster.Spec: no cluster policy is
	// the ordinary case on a fresh cluster, and dereferencing it there would
	// panic the reconciler on the first tenant that sets an override.
	overrideAllowed := true
	if cluster != nil {
		overrideAllowed = cluster.Spec.OverrideAllowed()
	}
	if !overrideAllowed && (override.Spec.Destination.IsSet() || override.Spec.Schedule != "") {
		return eff, fmt.Errorf(
			"this cluster does not permit tenants to override the backup policy (allowTenantOverride is false)")
	}

	if d := override.Spec.Destination; d.IsSet() {
		if err := d.Validate(); err != nil {
			return eff, fmt.Errorf("tenant backup policy: %w", err)
		}
		// Wholesale, not field by field: half of one endpoint's settings and
		// half of another's addresses no storage that exists.
		eff.Endpoint, eff.Region = d.Endpoint, d.Region
		eff.CredentialsSecret, eff.CredentialsNamespace = "", ""
		if d.CredentialsSecretRef != nil {
			eff.CredentialsSecret = d.CredentialsSecretRef.Name
			// The tenant's own namespace, always. A tenant naming a Secret it
			// does not own would read credentials it was never given.
			eff.CredentialsNamespace = TenantNamespace(tenant.Name)
		}
		if d.Bucket != "" {
			eff.Bucket = d.Bucket
		}
		eff.Overridden = true
	}

	switch {
	case override.Spec.SuspendSchedule:
		eff.Schedule = ""
	case override.Spec.Schedule != "":
		eff.Schedule = override.Spec.Schedule
	}
	if override.Spec.KeepLast != nil {
		eff.KeepLast = *override.Spec.KeepLast
	}
	return eff, nil
}
