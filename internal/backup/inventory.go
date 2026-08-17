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

// Package backup enumerates what a tenant is made of, and captures it.
//
// The enumeration half of this package is the single answer to "which stores
// does this app own, and what are they called". Three callers need that answer
// and must never disagree about it: provisioning creates the stores, purge
// destroys them, and export copies them. They used to each carry their own
// copy of the naming rules, which is how the PostgreSQL role name drifted from
// the database name and left a login role behind on every purge (see
// PostgresRole). A store nobody can name is a store nobody backs up, so the
// rules live here once.
package backup

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TenantNamespace returns the namespace a tenant's workloads run in.
func TenantNamespace(tenantName string) string {
	return "tenant-" + tenantName
}

// DatabaseName returns the relational database provisioned for a tenant + app.
// Honours spec.isolation.databasePrefix, defaulting to "{tenant}_", and
// replaces hyphens because they are not legal in an unquoted SQL identifier.
func DatabaseName(tenant *gentianov1alpha1.Tenant, app string) string {
	prefix := tenant.Name + "_"
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.DatabasePrefix != "" {
		prefix = tenant.Spec.Isolation.DatabasePrefix
	}
	return strings.ReplaceAll(prefix, "-", "_") + strings.ReplaceAll(app, "-", "_")
}

// PostgresRole returns the PostgreSQL login role for a tenant + app.
//
// The role is *not* named like the database: the database has hyphens replaced
// to satisfy identifier rules, the role joins the names verbatim, so profile
// "docmost-ce" in tenant "demo" owns database demo_docmost_ce as role
// demo_docmost-ce. Conflating the two is not hypothetical — purge did it, so
// DROP DATABASE matched while DROP ROLE never did, and every purged app left
// its login role behind along with any database that role owned.
func PostgresRole(tenantName, app string) string {
	return tenantName + "_" + app
}

// MariaDBUser returns the MariaDB user for a tenant + app. MariaDB is stricter
// than PostgreSQL here: both halves have hyphens replaced.
func MariaDBUser(tenantName, app string) string {
	return strings.ReplaceAll(tenantName, "-", "_") + "_" + strings.ReplaceAll(app, "-", "_")
}

// S3Bucket returns the object-storage bucket provisioned for a tenant + app.
// Honours spec.isolation.s3Prefix, defaulting to "{tenant}-".
func S3Bucket(tenant *gentianov1alpha1.Tenant, app string) string {
	prefix := tenant.Name + "-"
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.S3Prefix != "" {
		prefix = tenant.Spec.Isolation.S3Prefix
	}
	return s3Safe(prefix) + s3Safe(app)
}

// BackupBucket returns the bucket a tenant's export bundles are written to.
//
// It deliberately does not go through S3Bucket with a reserved app name: this
// bucket must never appear in the per-app inventory, or the next export would
// try to back up the previous one. AppBuckets is the only enumeration exports
// read, and it is built from kernelRequirements, which no profile can use to
// claim this name.
func BackupBucket(tenant *gentianov1alpha1.Tenant) string {
	prefix := tenant.Name + "-"
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.S3Prefix != "" {
		prefix = tenant.Spec.Isolation.S3Prefix
	}
	return s3Safe(prefix) + "gentian-backup"
}

// RedisACLUser returns the Redis ACL username for a tenant + app.
func RedisACLUser(tenantName, app string) string {
	return tenantName + "-" + app
}

// CNPGDatabaseCR returns the name of the CloudNativePG Database resource that
// provisions an app's database.
func CNPGDatabaseCR(tenantName, app string) string {
	return "db-" + tenantName + "-" + app
}

func s3Safe(value string) string {
	var b strings.Builder
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-':
			b.WriteRune(ch)
		case ch >= 'A' && ch <= 'Z':
			b.WriteRune(ch + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// Stores is the set of kernel-backed stores one app owns. It is derived purely
// from the profile's declared kernelRequirements — never from the app's name —
// so a new catalogue entry is captured correctly without touching this repo.
type Stores struct {
	// Database is the engine backing this app, or "" when it needs none.
	Database gentianov1alpha1.DatabaseEngine
	// S3 reports whether the app was given an object-storage bucket.
	S3 bool
	// Redis reports whether the app was given a cache. Caches are never
	// captured — a restored cache is at best useless and at worst stale — but
	// purge needs to know, so the field belongs to the shared inventory.
	Redis bool
}

// ProfileStores reports which kernel stores a profile declares.
func ProfileStores(profile *gentianov1alpha1.AppProfile) Stores {
	if profile == nil || profile.Spec.KernelRequirements == nil {
		return Stores{}
	}
	kr := profile.Spec.KernelRequirements
	var s Stores
	if kr.Database != nil {
		s.Database = kr.Database.Engine
	}
	if kr.Storage != nil && kr.Storage.S3 != nil {
		s.S3 = true
	}
	if kr.Cache != nil && kr.Cache.Engine == gentianov1alpha1.CacheEngineRedis {
		s.Redis = true
	}
	return s
}

// SidecarNames returns the declared sidecar names of a profile. Sidecars get
// their own OpenBao subtree under a synthetic "{app}-{sidecar}" key, so both
// purge and export have to walk them.
func SidecarNames(profile *gentianov1alpha1.AppProfile) []string {
	if profile == nil {
		return nil
	}
	out := make([]string, 0, len(profile.Spec.Sidecars))
	for _, sc := range profile.Spec.Sidecars {
		if sc.Name != "" {
			out = append(out, sc.Name)
		}
	}
	return out
}

// PVCBelongsToApp reports whether a claim was provisioned for this app.
//
// The label checks are exact; the trailing name check is a substring, which is
// deliberately broad — charts name volumes inconsistently and some ship none of
// the standard labels. Callers that *delete* what this matches must first ask
// OwnedByOtherRelease, because the substring is wide enough to reach a sibling
// app's volume when two profiles share a family.
func PVCBelongsToApp(pvc corev1.PersistentVolumeClaim, appName, family string) bool {
	if pvc.Labels["gentianos.io/app"] == appName {
		return true
	}
	if instance, ok := pvc.Labels["app.kubernetes.io/instance"]; ok && strings.HasPrefix(instance, appName) {
		return true
	}
	if name, ok := pvc.Labels["app.kubernetes.io/name"]; ok {
		if name == appName || (family != "" && name == family) {
			return true
		}
	}
	return strings.Contains(pvc.Name, appName) || (family != "" && strings.Contains(pvc.Name, family))
}

// OwnedByOtherRelease reports whether a Helm-managed object belongs to a
// release other than this app's, returning the release name for logging.
//
// For purge this is a veto: deleting an object out from under a live release is
// unrecoverable, because provider-helm reconciles release *state* and will not
// notice the object is gone. For export it is only a hint — capturing a
// neighbour's volume wastes space but destroys nothing — so export may choose
// to include what purge would skip.
func OwnedByOtherRelease(annotations map[string]string, appName string) (string, bool) {
	release := annotations["meta.helm.sh/release-name"]
	if release == "" || strings.Contains(release, appName) {
		return release, false
	}
	return release, true
}
