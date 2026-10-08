/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TenantNamespace returns the namespace a tenant's workloads run in.
//
// There is one rule and it is the Tenant's own (NamespaceName), which honours
// spec.isolation.namespace. Provisioning has always read it; export, restore,
// purge and the read of what uninstalled apps hold used to spell
// "tenant-<name>" for themselves, and would have looked in an empty namespace
// for a tenant placed anywhere else. They read this now, so the acts cannot
// disagree about where a tenant's workloads and volumes are.
func TenantNamespace(tenant *gentianov1alpha1.Tenant) string {
	return tenant.NamespaceName()
}

// TenantNames are the three names everything a tenant owns on the shared
// services is named from: its realm at the identity provider, and the
// prefixes of its databases and of its buckets. Two tenants with one of them
// in common do not have two of the thing: they have one, and each of them
// provisions into it, restores over it and, when it goes, destroys it.
type TenantNames struct {
	Realm          string
	DatabasePrefix string
	BucketPrefix   string
}

// NamesOf is a tenant's names: what its manifest states, or the platform's
// one rule for a tenant that states nothing -- the realm is the tenant's
// name, the database prefix the name and an underscore, the bucket prefix the
// name and a hyphen. As they are used: hyphens are not legal in a database's
// name and become underscores, and a bucket's name is lower case.
func NamesOf(tenant *gentianov1alpha1.Tenant) TenantNames {
	names := TenantNames{Realm: tenant.Name, DatabasePrefix: tenant.Name + "_", BucketPrefix: tenant.Name + "-"}
	if iso := tenant.Spec.Isolation; iso != nil {
		if iso.KeycloakRealm != "" {
			names.Realm = iso.KeycloakRealm
		}
		if iso.DatabasePrefix != "" {
			names.DatabasePrefix = iso.DatabasePrefix
		}
		if iso.S3Prefix != "" {
			names.BucketPrefix = iso.S3Prefix
		}
	}
	names.DatabasePrefix = strings.ReplaceAll(names.DatabasePrefix, "-", "_")
	names.BucketPrefix = s3Safe(names.BucketPrefix)
	return names
}

// RuleIsolation is the isolation block of a tenant named by the platform's
// one rule: what a tenant created through the director is written with.
func RuleIsolation(name string, mode gentianov1alpha1.IsolationMode) *gentianov1alpha1.TenantIsolation {
	return &gentianov1alpha1.TenantIsolation{
		Mode:           mode,
		KeycloakRealm:  name,
		DatabasePrefix: name + "_",
		S3Prefix:       name + "-",
	}
}

// ErrNamesTaken is a tenant one of whose names another tenant already uses.
var ErrNamesTaken = errors.New("a name this tenant would use is another tenant's")

// NamesTaken answers what is wrong with a tenant's names beside the tenants
// a cluster already has, or nil. A tenant whose realm, database prefix or
// bucket prefix is another tenant's is refused before it exists: once it
// does, provisioning adopts the other's realm, databases and buckets as its
// own, a restore replaces them, and its deletion destroys them.
//
// It is asked wherever a tenant comes into being -- the director, before it
// commits one, and the admission webhook, before the cluster stores one --
// and is one function so that they cannot come to different answers.
func NamesTaken(candidate *gentianov1alpha1.Tenant, others []gentianov1alpha1.Tenant) error {
	mine := NamesOf(candidate)
	var clashes []string
	for i := range others {
		other := &others[i]
		if other.Name == candidate.Name {
			continue
		}
		theirs := NamesOf(other)
		if mine.Realm == theirs.Realm {
			clashes = append(clashes, fmt.Sprintf("the realm %q is tenant %s's", mine.Realm, other.Name))
		}
		if mine.DatabasePrefix == theirs.DatabasePrefix {
			clashes = append(clashes, fmt.Sprintf("the database prefix %q is tenant %s's", mine.DatabasePrefix, other.Name))
		}
		if mine.BucketPrefix == theirs.BucketPrefix {
			clashes = append(clashes, fmt.Sprintf("the bucket prefix %q is tenant %s's", mine.BucketPrefix, other.Name))
		}
	}
	if len(clashes) == 0 {
		return nil
	}
	sort.Strings(clashes)
	return fmt.Errorf("%w: tenant %q is refused, because %s. Two tenants with one of these in common share the thing itself; "+
		"give the tenant names of its own (spec.isolation), or leave them out so that they follow from its name",
		ErrNamesTaken, candidate.Name, strings.Join(clashes, "; "))
}

// DatabaseName returns the relational database provisioned for a tenant + app.
// Honours spec.isolation.databasePrefix, defaulting to "{tenant}_", and
// replaces hyphens because they are not legal in an unquoted SQL identifier.
func DatabaseName(tenant *gentianov1alpha1.Tenant, app string) string {
	return NamesOf(tenant).DatabasePrefix + strings.ReplaceAll(app, "-", "_")
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
	return NamesOf(tenant).BucketPrefix + s3Safe(app)
}

// BackupBucket returns the bucket a tenant's export bundles are written to.
//
// It deliberately does not go through S3Bucket with a reserved app name: this
// bucket must never appear in the per-app inventory, or the next export would
// try to back up the previous one. AppBuckets is the only enumeration exports
// read, and it is built from kernelRequirements, which no profile can use to
// claim this name.
func BackupBucket(tenant *gentianov1alpha1.Tenant) string {
	return NamesOf(tenant).BucketPrefix + "gentian-backup"
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

// DesktopStore is the name the desktop's database, role and vault record are
// kept under for a tenant. It is shaped like an app's name and is not one: no
// catalogue entry is called this and nothing installs or uninstalls it, so a
// tenant always holds these stores and never "retains" them.
const DesktopStore = "shell"

// backupStore is the same for the tenant's backup bucket.
const backupStore = "gentian-backup"

// IsPlatformStore reports whether a name addresses stores the platform keeps
// for a tenant itself rather than for an app the tenant installed. A purge of
// an app must refuse such a name -- it would drop the desktop's database --
// and a list of what uninstalled apps left behind must not show it.
func IsPlatformStore(name string) bool {
	return name == DesktopStore || name == backupStore
}

// AppRelease is the Helm release an app is installed as in its tenant's
// namespace: one name for as long as the app is the tenant's, whichever
// install of it this is. The app Composition sets it as the release's
// external name. Being stable is what lets a volume claim kept by one
// uninstall be found, and taken over, by the next install.
func AppRelease(app string) string {
	return app + "-release"
}

// ExtensionRelease is the Helm release of one extension (sidecar) of an app.
func ExtensionRelease(app, extension string) string {
	return app + "-" + extension + "-release"
}

// DirectRelease is the Helm release of a chart the component reconciler
// delivers itself, without the app Composition. The Release object is
// cluster-scoped and carries the namespace in its name; the Helm release
// takes the same name.
func DirectRelease(namespace, app string) string {
	return namespace + "-" + app
}

// IsAppRelease reports whether a Helm release in the tenant's namespace is
// one of this app's. namespace is that namespace (TenantNamespace).
//
// extensions are the app's declared extensions. known says whether they are:
// when the app's profile is gone they are not, and any release shaped like an
// extension's ("<app>-…-release") is taken to be one. Callers that destroy on
// the strength of that wider reading must first rule out that the release is
// another installed app's.
func IsAppRelease(release, namespace, app string, extensions []string, known bool) bool {
	if release == AppRelease(app) || release == DirectRelease(namespace, app) {
		return true
	}
	if !known {
		return strings.HasPrefix(release, app+"-") && strings.HasSuffix(release, "-release")
	}
	for _, ext := range extensions {
		if release == ExtensionRelease(app, ext) {
			return true
		}
	}
	return false
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
func ProfileStores(profile *gentianov1alpha1.ComponentProfile) Stores {
	if profile == nil || profile.Services() == nil {
		return Stores{}
	}
	kr := profile.Services()
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
func SidecarNames(profile *gentianov1alpha1.ComponentProfile) []string {
	if profile == nil {
		return nil
	}
	out := make([]string, 0, len(profile.Spec.Extensions))
	for _, sc := range profile.Spec.Extensions {
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
// whether the claim records a Helm release and, if it does, whether that
// release is the app's (IsAppRelease): the substring is wide enough to reach a
// sibling app's volume when two profiles share a family, or when one app's
// name begins another's.
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

// --- whose a volume claim is ------------------------------------------------

// TenantApps are the apps and add-ons the tenant has.
func TenantApps(tenant *gentianov1alpha1.Tenant) map[string]bool {
	out := map[string]bool{}
	for _, a := range tenant.Spec.Apps {
		out[a.Profile] = true
		for _, addon := range a.Addons {
			out[addon] = true
		}
	}
	return out
}

// OwnedKeys are the names an app's stores are kept under: its own, and one
// per extension, which the app Composition keys "{app}-{extension}". An
// extension's vault path, its access group and its Helm release are all named
// from its key, exactly as the app's own are from the app's name.
//
// A key that is itself an app or add-on of the tenant is left out. Nothing
// stops an app being called what another app's extension key spells, and what
// an installed app holds is not another app's to destroy or to copy.
func OwnedKeys(tenant *gentianov1alpha1.Tenant, app string, extensions []string) []string {
	inUse := TenantApps(tenant)
	keys := []string{app}
	for _, ext := range extensions {
		if key := app + "-" + ext; !inUse[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

// ClaimRelease is the Helm release a claim is on record as belonging to: the
// one Helm annotated it with, for a claim the chart templated; the one its
// instance label names, for a claim a StatefulSet of the chart made.
func ClaimRelease(pvc corev1.PersistentVolumeClaim) string {
	if release := pvc.Annotations["meta.helm.sh/release-name"]; release != "" {
		return release
	}
	return pvc.Labels["app.kubernetes.io/instance"]
}

// OwnsRelease reports whether a Helm release in the tenant's namespace is
// this app's and nobody else's.
func OwnsRelease(tenant *gentianov1alpha1.Tenant, app string, profile *gentianov1alpha1.ComponentProfile, release string) bool {
	// The release of an app or add-on the tenant has is that app's, whatever
	// else its name resembles.
	if key, ok := strings.CutSuffix(release, "-release"); ok && key != app && TenantApps(tenant)[key] {
		return false
	}
	extensions := make([]string, 0)
	for _, key := range OwnedKeys(tenant, app, SidecarNames(profile))[1:] {
		extensions = append(extensions, strings.TrimPrefix(key, app+"-"))
	}
	return IsAppRelease(release, TenantNamespace(tenant), app, extensions, profile != nil)
}

// AppVolumes are the claims in the tenant's namespace that are this app's:
// what an export copies, what a purge deletes, and what the read of retained
// data reports. One rule for the three, so that what is reported as kept is
// what a purge would destroy and what a bundle carries is the app's and
// nobody else's.
//
// A claim is the app's when it matches by label or by name (PVCBelongsToApp)
// and is not on record as another release's. The second half is a veto over
// the first. The match falls back to a name substring, which reaches a
// sibling's volume when two profiles share a chart (nextcloud-base-ce matches
// anything containing "nextcloud") or when one app's name begins another's.
// For a purge that would delete a running app's volume out from under it,
// which nothing puts back; for an export it would put one app's files into
// another app's part of a bundle, and a restore would then write them over
// the sibling's.
//
// So a claim that names a release is the app's only if the release is: the
// app's own, one of its declared extensions', or the one a chart delivered
// without the app Composition is installed as. Those names are exact
// (AppRelease and its neighbours), which is also how a claim an uninstall
// kept is known to be this app's: it still carries its release. When the
// profile is gone the extensions are not known, and any release shaped like
// one of the app's counts -- unless it is the release of an app the tenant
// has. A claim that names no release is matched by label and name alone.
//
// The chart's name is what a claim's app.kubernetes.io/name is likely to be
// when it is not the install's own name. Without a profile it is empty, which
// only narrows the match. vetoed says, per claim left out, whose release it
// records.
func AppVolumes(claims []corev1.PersistentVolumeClaim, tenant *gentianov1alpha1.Tenant, app string, profile *gentianov1alpha1.ComponentProfile) (own []string, vetoed map[string]string) {
	vetoed = map[string]string{}
	chart := ""
	if c := profile.Chart(); c != nil {
		chart = c.Name
	}
	for _, pvc := range claims {
		if !PVCBelongsToApp(pvc, app, chart) {
			continue
		}
		if release := ClaimRelease(pvc); release != "" && !OwnsRelease(tenant, app, profile, release) {
			vetoed[pvc.Name] = release
			continue
		}
		own = append(own, pvc.Name)
	}
	return own, vetoed
}
