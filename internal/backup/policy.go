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
	"slices"

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

	// Recipients are the age public keys bundles are encrypted to. Empty means
	// the cluster's own key, which the export controller resolves for itself;
	// non-empty means the platform cannot read what it writes.
	Recipients []string

	// Overridden records that the tenant, not the cluster, chose the
	// destination — the difference between bundles the platform can reach and
	// bundles it may not be able to.
	Overridden bool
}

// PlatformStorage reports whether bundles go to the platform's own MinIO.
func (e Effective) PlatformStorage() bool { return e.Endpoint == "" }

// ApplyExportDestination narrows a resolved policy to what one export asked
// for.
//
// Applied after ResolveEffective rather than inside it, because the policy is
// the standing arrangement and this is one export's departure from it. Keeping
// them separate is what lets status report both: what the policy says, and
// what this bundle actually did.
func ApplyExportDestination(
	eff Effective,
	d *gentianov1alpha1.ExportDestination,
	tenant *gentianov1alpha1.Tenant,
	exportName string,
) Effective {
	switch d.Resolved() {
	case gentianov1alpha1.ExportDestinationPlatform:
		// Everything that addresses somewhere else is cleared together. Half a
		// destination -- an external endpoint with the platform's credential,
		// say -- addresses no storage that exists.
		eff.Endpoint, eff.Region = "", ""
		eff.CredentialName, eff.CredentialSecret = "", ""
		eff.Bucket = BackupBucket(tenant)
		eff.Overridden = true

	case gentianov1alpha1.ExportDestinationCustom:
		eff.Endpoint, eff.Region = d.Endpoint, d.Region
		if d.Bucket != "" {
			eff.Bucket = d.Bucket
		}
		if d.ResolvedCredentialSource() == gentianov1alpha1.ExportCredentialTransient {
			// No CredentialName: a requirement is a standing arrangement
			// someone administers and rotates, and these keys are used once.
			// The Secret is staged beside the capture Jobs and discarded with
			// the export.
			eff.CredentialName = ""
			eff.CredentialSecret = ExportCredentialSecretName(exportName)
		}
		// credentialSource: managed keeps whatever the policy resolved, which
		// is the workspace's own destination credential — already materialised
		// beside the capture Jobs by ESO. Only the endpoint moves; nothing is
		// copied, and no keys pass through a spec to get here.
		eff.Overridden = true
	}
	return eff
}

// ExportCredentialSecretName is the staged copy's name, beside the capture
// Jobs. Derived from the export rather than the requester's Secret name, so a
// tenant cannot aim it at a Secret in the kernel namespace it does not own —
// which is why the export's name is the argument and the reference is not.
func ExportCredentialSecretName(exportName string) string {
	return "tenant-export-destination-" + exportName
}

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
		if p.Spec.Encryption.IsSet() {
			// Replace, never append. A tenant that names its own key is asking
			// for bundles nobody else can read; quietly adding the cluster's
			// alongside would answer a different question than the one asked.
			eff.Recipients = slices.Clone(p.Spec.Encryption.Recipients)
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
		override.Spec.Retention.IsSet() || override.Spec.Encryption.IsSet()
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
