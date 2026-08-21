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

// The keys an S3 destination needs, and the name they are stored under.
const (
	// DestinationAccessKeyField and DestinationSecretKeyField are the field
	// names at the credential's vault path, and the keys of the Secret ESO
	// materialises from it.
	DestinationAccessKeyField = "accessKey"
	DestinationSecretKeyField = "secretKey" //nolint:gosec // Field name, not a credential.

	// clusterDestinationCredential is the requirement name for the cluster's
	// own destination; a tenant's is suffixed with the tenant name.
	clusterDestinationCredential = "backup-destination"
)

// DestinationCredentialName is the CredentialRequirement carrying an
// endpoint's keys. Derived rather than configured: a policy that could name
// any requirement would let a tenant point at one it does not own.
func DestinationCredentialName(scope, tenant string) string {
	if scope == "tenant" && tenant != "" {
		return clusterDestinationCredential + "-" + tenant
	}
	return clusterDestinationCredential
}

// DestinationVaultPath is where those keys live in OpenBao.
//
// The tenant path sits under the tenant's own subtree, which is what makes the
// existing OpenBao policy split do the access control: a tenant admin reaches
// gentian-os/tenants/<their tenant>/… and nothing above it.
func DestinationVaultPath(scope, tenant string) string {
	if scope == "tenant" && tenant != "" {
		return fmt.Sprintf("gentian-os/tenants/%s/backup/destination", tenant)
	}
	return "gentian-os/kernel/backup/destination"
}

// DestinationSecretName is the Secret ESO materialises for capture Jobs.
func DestinationSecretName(scope, tenant string) string {
	return "backup-destination-" + scopeTag(scope, tenant)
}

func scopeTag(scope, tenant string) string {
	if scope == "tenant" && tenant != "" {
		return tenant
	}
	return "cluster"
}

// Effective is what a tenant's backups actually do, after the cluster policy
// and the tenant's own have been merged.
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

	// CredentialName is the CredentialRequirement the destination's keys come
	// from, and CredentialSecret the Secret ESO materialises from it. Both are
	// empty for the platform's own storage, which needs no requirement.
	CredentialName   string
	CredentialSecret string

	// Schedule is a cron expression in UTC; empty means no scheduled backups.
	Schedule  string
	Retention gentianov1alpha1.BackupRetention

	// Overridden records that the tenant, not the cluster, chose the
	// destination — the difference between bundles the platform can reach and
	// bundles it may not be able to.
	Overridden bool
}

// PlatformStorage reports whether bundles go to the platform's own MinIO.
func (e Effective) PlatformStorage() bool { return e.Endpoint == "" }

// ResolveEffective merges the cluster policy with a tenant's own.
//
// A tenant policy is refused rather than silently ignored when the cluster
// forbids overrides: an admin who sets a destination and sees bundles going
// somewhere else has been misled, which is worse than being told no.
func ResolveEffective(
	tenant *gentianov1alpha1.Tenant,
	cluster *gentianov1alpha1.BackupPolicy,
	override *gentianov1alpha1.BackupPolicy,
) (Effective, error) {
	eff := Effective{Bucket: BackupBucket(tenant)}

	apply := func(p *gentianov1alpha1.BackupPolicy) {
		if d := p.Spec.Destination; d.IsSet() {
			// Wholesale, not field by field: half of one endpoint's settings
			// and half of another's addresses no storage that exists.
			eff.Endpoint, eff.Region = d.Endpoint, d.Region
			eff.CredentialName, eff.CredentialSecret = "", ""
			if d.NeedsCredential() {
				eff.CredentialName = DestinationCredentialName(p.Spec.Scope, p.Spec.Tenant)
				eff.CredentialSecret = DestinationSecretName(p.Spec.Scope, p.Spec.Tenant)
			}
			if d.Bucket != "" {
				eff.Bucket = d.Bucket
			}
		}
		switch {
		case p.Spec.SuspendSchedule:
			eff.Schedule = ""
		case p.Spec.Schedule != "":
			eff.Schedule = p.Spec.Schedule
		}
		if p.Spec.Retention.IsSet() {
			eff.Retention = *p.Spec.Retention
		}
	}

	if cluster != nil {
		apply(cluster)
	}
	if override == nil {
		return eff, nil
	}

	// Guarded rather than reaching through cluster.Spec: no cluster policy is
	// the ordinary case on a fresh cluster, and dereferencing it there would
	// panic the reconciler on the first tenant that states one.
	overrideAllowed := true
	if cluster != nil {
		overrideAllowed = cluster.Spec.OverrideAllowed()
	}
	states := override.Spec.Destination.IsSet() ||
		override.Spec.Schedule != "" || override.Spec.SuspendSchedule ||
		override.Spec.Retention.IsSet()
	if !overrideAllowed && states {
		return eff, fmt.Errorf(
			"this cluster does not permit tenants to set their own backup policy (allowTenantOverride is false)")
	}

	before := eff.Endpoint
	apply(override)
	if override.Spec.Destination.IsSet() || eff.Endpoint != before {
		eff.Overridden = true
	}
	return eff, nil
}
