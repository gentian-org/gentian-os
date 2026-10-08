/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/modelgateway"
)

// One inventory, one order.
//
// Five acts work on what an app of a tenant owns: provisioning makes it,
// export and backup copy it, uninstalling leaves it, a purge of the app
// destroys it, and deleting the tenant destroys it for every app at once.
// They must agree on what "it" is. This file is where that is written down:
// the kinds of thing an app owns, in the order provisioning makes them, and
// for each kind what every act does with it. Teardown is that order
// reversed, and nothing else defines one.
//
// The names of the things themselves are the functions in inventory.go, and
// the destruction of each store is a Job built below. The purge of one app
// and the deletion of a tenant run the same Jobs with the same scripts: the
// first waits for each in one request, the second creates them and comes
// back until they are done.

// Kind is one kind of thing an app owns.
type Kind string

const (
	// KindCredentials is what the vault holds for the app: the credentials
	// provisioning seeds for each store below, and the secrets generated for
	// the app and for its extensions.
	KindCredentials Kind = "credentials"
	// KindAccessGroup is the identity provider's group that says who may use
	// the app, and its memberships.
	KindAccessGroup Kind = "accessGroup"
	// KindDatabase is the app's relational database and its login role.
	KindDatabase Kind = "database"
	// KindObjectStorage is the app's bucket, with the user and the policy
	// made for it.
	KindObjectStorage Kind = "objectStorage"
	// KindCache is the app's user in the shared cache.
	KindCache Kind = "cache"
	// KindSignInScope is the client scope the app's sign-in pack describes,
	// with its protocol mappers, in the tenant's realm. It is the realm's
	// and not the client's, so it does not go when the client does.
	KindSignInScope Kind = "signInScope"
	// KindModelAccess is the key an app that declared the model gateway
	// calls models with there, on a cluster that serves models.
	KindModelAccess Kind = "modelAccess"
	// KindSignInClient is the app's OIDC or SAML client in the tenant's
	// realm, with what is part of it: its client role, its default-scope
	// assignments, and the access group's mapping to that role.
	KindSignInClient Kind = "signInClient"
	// KindWorkloads is the app's Helm release: everything its chart renders
	// except the volume claims.
	KindWorkloads Kind = "workloads"
	// KindFiles is the volume claims the app's chart creates.
	KindFiles Kind = "files"
	// KindRecords is what provisioning left in the cluster about the app:
	// its Jobs, their pods and the Secrets labelled with it.
	KindRecords Kind = "provisioningRecords"
)

// What an act does with a kind.
type Disposition string

const (
	// Creates: the act makes it.
	Creates Disposition = "creates"
	// Keeps: the act leaves it in place.
	Keeps Disposition = "keeps"
	// Removes: the act takes it away; nothing a person stored goes with it.
	Removes Disposition = "removes"
	// Destroys: the act deletes it, and what it held.
	Destroys Disposition = "destroys"
	// Carries: the act copies it into a bundle.
	Carries Disposition = "carries"
	// Omits: the act deliberately does not copy it. The rule says why.
	Omits Disposition = "omits"
	// Gone: there is nothing left of it by the time the act runs.
	Gone Disposition = "gone"
)

// KindRule says what each act does with one kind.
type KindRule struct {
	Kind Kind
	// Provision is always Creates; MadeBy says where.
	MadeBy string
	// Export is what an export and a backup do: they are one mechanism, and
	// a restore or an import puts back exactly what was carried.
	Export Disposition
	// ExportNote says how it is carried, or why it is not.
	ExportNote string
	// Uninstall is what removing the app from the tenant does.
	Uninstall Disposition
	// AppPurge is what purging the uninstalled app does.
	AppPurge Disposition
	// TenantDelete is what deleting the tenant with deletionPolicy: Delete
	// does. With Retain it keeps every kind and removes only the workloads.
	TenantDelete Disposition
	// TenantDeleteNote says by what, where it is not the app's own step.
	TenantDeleteNote string
	// FoundBy says how the kind is found for an app that is no longer
	// installed: by the read of what uninstalled apps hold, by a purge and by
	// the deletion of the tenant. Uninstalling keeps the kind, the app has
	// left the tenant's manifest, and something durable has to say the kind
	// is still there -- a Job that expires does not. Empty for a kind
	// uninstalling removes.
	FoundBy string
}

// AppKinds is every kind of thing an app owns, in the order provisioning
// makes them: the credentials first, because each store is created with the
// password the vault already holds; then the group and the stores; then the
// client, the release and, with the release, the files. The records come
// first because the first of them is written before anything else exists,
// and so go last in a teardown -- whose own Jobs are records too.
//
// A kind added here without saying what export and each teardown do with it
// fails the package's tests, which is the point of having one table.
var AppKinds = []KindRule{
	{
		Kind: KindRecords, MadeBy: "every provisioning Job, and the operator's labelled Secrets, from the first step on",
		Export: Omits, ExportNote: "not data; re-made by provisioning",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		FoundBy: "the tenant's and the app's labels on each object",
	},
	{
		Kind: KindCredentials, MadeBy: "the tenant reconciler's seeder, and the app Composition for generated secrets",
		Export: Omits, ExportNote: "a bundle holds no stored credential; the tenant restored into has its own, seeded when it was provisioned, and a restore changes none; what a person entered has to be entered again",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		TenantDeleteNote: "with the tenant's whole vault subtree",
		FoundBy:          "the names below the tenant's apps path in the vault",
	},
	{
		Kind: KindAccessGroup, MadeBy: "the tenant's identity Job, from Tenant.spec.apps",
		Export: Carries, ExportNote: "inside the realm export",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		TenantDeleteNote: "with the realm",
		FoundBy:          "the group's name in the tenant's realm",
	},
	{
		Kind: KindSignInScope, MadeBy: "the tenant's identity Job, from the OIDC pack the app's profile names",
		Export: Omits, ExportNote: "configuration with nothing of a person's in it; the identity Job makes it again when the app is installed",
		// Kept at an uninstall although it means nothing without the client:
		// taking it away there would need the uninstall to talk to the identity
		// provider and to fail when it cannot, and an uninstall is a commit
		// the cluster follows, with nobody waiting on an answer. A purge is
		// asked for, answers, and already talks to the provider for the group.
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		TenantDeleteNote: "with the realm",
		FoundBy:          "the pack the app's profile names, which names the scope",
	},
	{
		Kind: KindDatabase, MadeBy: "the tenant reconciler: a role Job and a CloudNativePG Database, or a MariaDB setup Job",
		Export: Carries, ExportNote: "a dump of the provisioned database, and of every other database that is the app's, which is what a purge drops: on PostgreSQL the ones the app's role owns, on MariaDB the ones named with the provisioned name as a prefix",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		FoundBy: "the tenant's record of what was provisioned; for PostgreSQL also the CloudNativePG Database object",
	},
	{
		Kind: KindObjectStorage, MadeBy: "the tenant reconciler's bucket Job",
		Export: Carries, ExportNote: "the bucket's objects; a restore makes the bucket, its user and its policy with the code install uses, then writes the objects",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		FoundBy: "the tenant's record of what was provisioned",
	},
	{
		Kind: KindCache, MadeBy: "the tenant reconciler's ACL Job",
		Export: Omits, ExportNote: "a restored cache is at best useless and at worst stale",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		FoundBy: "the tenant's record of what was provisioned",
	},
	{
		Kind: KindModelAccess, MadeBy: "the tenant reconciler, at the model gateway, for an app -- or a component the platform places on the tenant -- whose profile declares it (requires.services.llm), on a cluster that serves models",
		Export: Omits, ExportNote: "a credential; the tenant restored into has its own, registered when the app is installed there",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		TenantDeleteNote: "and the tenant's team at the gateway",
		FoundBy:          "the tenant's record of what was provisioned",
	},
	{
		Kind: KindSignInClient, MadeBy: "the app Composition, or the tenant's identity Job",
		Export: Carries, ExportNote: "inside the realm export",
		// The client role, the client's default-scope assignments and the
		// group's mapping to the role are composed with an Orphan policy, so
		// Crossplane deletes none of them itself. Each is part of the client
		// in the identity provider and goes when the client is deleted, which
		// the Composition does (deletionPolicy: Delete on the Client).
		Uninstall: Removes, AppPurge: Gone, TenantDelete: Destroys,
		TenantDeleteNote: "with the realm",
	},
	{
		Kind: KindWorkloads, MadeBy: "the Helm release the app Composition or the component reconciler writes",
		Export: Omits, ExportNote: "re-made from the app's profile; the manifest records the chart version, the digest and the releases, and a restore checks the installed build against them",
		Uninstall: Removes, AppPurge: Gone, TenantDelete: Removes,
	},
	{
		Kind: KindFiles, MadeBy: "the app's chart, as volume claims of its release",
		Export: Carries, ExportNote: "an archive of each volume claim that is the app's, by the rule a purge deletes by (AppVolumes)",
		Uninstall: Keeps, AppPurge: Destroys, TenantDelete: Destroys,
		TenantDeleteNote: "with the tenant's namespace",
		FoundBy:          "the volume claims in the tenant's namespace, by the release each records (AppVolumes)",
	},
}

// TenantRule is one thing a tenant has that is no app's, and what deleting
// the tenant does with it.
type TenantRule struct {
	// What is the thing, as a person would name it.
	What string
	// MadeBy says what makes it.
	MadeBy string
	// Export says what an export does with it.
	Export string
	// Retain and Delete say what deleting the tenant does with it under each
	// deletion policy, and by what.
	Retain string
	Delete string
}

// TenantOwned is what deleting a tenant removes besides what its apps own.
// The list exists because several of these lie outside everything a deletion
// sweeps by default -- outside the tenant's namespace, its realm and its
// vault subtree, and without the tenant's label -- which is how each of them
// came to be left behind. Written down, each has to say what removes it.
var TenantOwned = []TenantRule{
	{
		What: "the tenant's namespace, with every workload and volume in it", MadeBy: "the tenant's Composition",
		Export: "volumes: per app; workloads: not carried",
		Retain: "kept; its Components and the operator's quota, limits and network policy are removed",
		Delete: "deleted, and the deletion waits until it is gone",
	},
	{
		What: "the tenant's realm", MadeBy: "the tenant's identity Job and its Composition",
		Export: "carried: configuration, people and memberships, no passwords",
		Retain: "disabled",
		Delete: "deleted; never when it is the kernel realm, which a tenant only adopts",
	},
	{
		What: "the client the tenant's realm signs in to the kernel realm as (broker-<realm>), and the mapper on it", MadeBy: "the tenant's identity Job; the mapper by its Composition",
		Export: "not carried; made again with the realm",
		Retain: "kept",
		Delete: "removed from the kernel realm, by the Job that deletes the realm",
	},
	{
		What: "the tenant's vault subtree", MadeBy: "the tenant reconciler's seeder, and people",
		Export: "not carried: a bundle holds no stored credential",
		Retain: "kept",
		Delete: "deleted; an operator with no vault that was not told to run without one fails here",
	},
	{
		What: "the tenant's team at the model gateway", MadeBy: "the tenant reconciler",
		Export: "not carried",
		Retain: "kept",
		Delete: "removed, after the keys of its apps",
	},
	{
		What: "the tenant's mail routing, its submission and IMAP credentials, and its mail DNS records (DNSEndpoint mail-<tenant>)", MadeBy: "the tenant reconciler, in the mail namespaces",
		Export: "not carried",
		Retain: "removed: a retired tenant must stop receiving and sending",
		Delete: "removed; and its DKIM key and SMTP credentials",
	},
	{
		What: "the tenant's edge: gateway, routes, wildcard certificate, edge routes and edge DNS records", MadeBy: "the tenant reconciler",
		Export: "not carried",
		Retain: "removed",
		Delete: "removed; a route that cannot be removed fails the deletion",
	},
	// The two below are on the list for what it is for: each lies outside
	// everything a deletion sweeps, and nothing removes either. They say so.
	// Deleting mail is a kind of destruction no act performs today; whether a
	// tenant's deletion should is not decided here.
	{
		What: "the tenant's mailboxes: the mail its people received and filed, on the mail server's own volume", MadeBy: "the mail server, as mail arrives; where the cluster runs its own",
		Export: "not carried: no bundle holds mail",
		Retain: "kept; the tenant's addresses stop receiving",
		Delete: "NOT removed: nothing deletes a mailbox, and they stay on the mail server's volume after the tenant is gone",
	},
	{
		What: "the entries in the rights store that say who is the tenant's member (membership tuples)", MadeBy: "the membership service, from the identity provider's events",
		Export: "not carried: they are derived again from the realm's people",
		Retain: "kept",
		Delete: "NOT removed: the tenant's app grants go, the entries naming people as its members stay",
	},
	{
		What: "the tenant's backup bucket", MadeBy: "the first export",
		Export: "it is where exports go",
		Retain: "kept",
		Delete: "destroyed, unless the tenant keeps its bundles (keepBundles)",
	},
	{
		What: "the record of what was provisioned", MadeBy: "the tenant reconciler",
		Export: "not carried",
		Retain: "kept, because the stores are",
		Delete: "deleted, last",
	},
}

// ProvisionOrder is the kinds in the order they are made.
func ProvisionOrder() []Kind {
	out := make([]Kind, 0, len(AppKinds))
	for _, rule := range AppKinds {
		out = append(out, rule.Kind)
	}
	return out
}

// TeardownOrder is the order a teardown works in: provisioning's, reversed.
// What was made last goes first, so nothing is destroyed while something
// made after it still depends on it -- the files before the stores, the
// stores before the group, and the credentials last, which also means a
// teardown that stops half-way can still be found by what is left of them.
func TeardownOrder() []Kind {
	out := ProvisionOrder()
	slices.Reverse(out)
	return out
}

// AppPurgeOrder is the kinds a purge of an uninstalled app destroys, in
// teardown order.
func AppPurgeOrder() []Kind {
	var out []Kind
	for _, kind := range TeardownOrder() {
		if RuleFor(kind).AppPurge == Destroys {
			out = append(out, kind)
		}
	}
	return out
}

// RuleFor returns the rule of a kind. It panics on a kind that has none:
// a kind nobody wrote a rule for is a programming error, not a state.
func RuleFor(kind Kind) KindRule {
	for _, rule := range AppKinds {
		if rule.Kind == kind {
			return rule
		}
	}
	panic("backup: no rule for kind " + string(kind))
}

// --- what one app owns ------------------------------------------------------

// AppInventory names everything one app of one tenant owns, kind by kind.
// Provisioning creates these names, export reads them and both teardowns
// destroy them; none of the three spells one out for itself.
type AppInventory struct {
	Tenant string
	App    string
	// Stores are the kernel stores the app's profile declares.
	Stores Stores
	// Database and DatabaseUser are the relational database and its login
	// (the PostgreSQL role, or the MariaDB user). Empty without a database.
	Database     string
	DatabaseUser string
	// DatabaseRecord is the CloudNativePG Database object that records a
	// PostgreSQL database. Empty for another engine.
	DatabaseRecord string
	// Bucket is the object-storage bucket. Empty without one.
	Bucket string
	// CacheUser is the user in the shared cache. Empty without one.
	CacheUser string
	// ModelKey is the alias the app's key is registered under at the model
	// gateway. Empty for an app whose profile does not declare the gateway.
	ModelKey string
	// Keys are the names the app's vault paths and access groups are kept
	// under: the app's own, and "{app}-{extension}" per declared extension.
	Keys []string
	// Releases are the Helm releases the app is installed as.
	Releases []string
	// Chart is the name of the app's chart, which a volume claim may carry
	// in place of the app's name.
	Chart string
}

// InventoryOf names what an app owns. The profile says which stores it has;
// with a nil profile only what every app has is named.
func InventoryOf(tenant *gentianov1alpha1.Tenant, app string, profile *gentianov1alpha1.ComponentProfile) AppInventory {
	inv := AppInventory{Tenant: tenant.Name, App: app, Stores: ProfileStores(profile), Keys: []string{app}}
	switch inv.Stores.Database {
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		inv.Database = DatabaseName(tenant, app)
		inv.DatabaseUser = PostgresRole(tenant.Name, app)
		inv.DatabaseRecord = CNPGDatabaseCR(tenant.Name, app)
	case gentianov1alpha1.DatabaseEngineMariaDB:
		inv.Database = DatabaseName(tenant, app)
		inv.DatabaseUser = MariaDBUser(tenant.Name, app)
	}
	if inv.Stores.S3 {
		inv.Bucket = S3Bucket(tenant, app)
	}
	if inv.Stores.Redis {
		inv.CacheUser = RedisACLUser(tenant.Name, app)
	}
	if DeclaresModelAccess(profile) {
		inv.ModelKey = modelgateway.KeyAlias(tenant.Name, app)
	}
	inv.Releases = []string{AppRelease(app), DirectRelease(TenantNamespace(tenant), app)}
	for _, ext := range SidecarNames(profile) {
		inv.Keys = append(inv.Keys, app+"-"+ext)
		inv.Releases = append(inv.Releases, ExtensionRelease(app, ext))
	}
	if chart := profile.Chart(); chart != nil {
		inv.Chart = chart.Name
	}
	return inv
}

// DeclaresModelAccess reports whether a profile declares that it calls models
// through the platform's gateway (requires.services.llm). It is no kernel
// store -- nothing of it is copied and no Job destroys it -- so it is not a
// field of Stores; it is a kind an app owns all the same, and the inventory
// names it.
func DeclaresModelAccess(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil && profile.Services() != nil && profile.Services().LLM != nil
}

// --- making a store ---------------------------------------------------------

// ObjectStorageProvisionContainer makes an app's bucket and, when it is
// given the key pair the vault holds for the app, the user and the policy
// the app reads the bucket with. It is the one piece of code that does:
// install runs it as the bucket Job, and a restore runs it before it writes
// a bucket's objects back.
//
// The key pair is passed in because the operator seeds it and a Job cannot
// read the vault. Without one only the bucket is made, which is what install
// does on a cluster with no vault.
func ObjectStorageProvisionContainer(name, bucket, accessKey, secretKey string) corev1.Container {
	no := false
	c := corev1.Container{
		Name:    name,
		Image:   MinIOClientImage,
		Command: []string{"/bin/sh", "-c", objectStorageProvisionScript(bucket)},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &no,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Env: append(PlatformStorageEnv(), corev1.EnvVar{Name: "BUCKET_NAME", Value: bucket}),
	}
	if accessKey != "" && secretKey != "" {
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "APP_ACCESS_KEY", Value: accessKey},
			corev1.EnvVar{Name: "APP_SECRET_KEY", Value: secretKey},
		)
	}
	return c
}

func objectStorageProvisionScript(bucket string) string {
	return fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mc mb --ignore-existing "gentian/%[1]s"
mc anonymous set none "gentian/%[1]s"
if [ -n "${APP_ACCESS_KEY:-}" ] && [ -n "${APP_SECRET_KEY:-}" ]; then
  mc admin user remove gentian "${APP_ACCESS_KEY}" >/dev/null 2>&1 || true
  mc admin user add gentian "${APP_ACCESS_KEY}" "${APP_SECRET_KEY}"
  printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::%[1]s","arn:aws:s3:::%[1]s/*"]}]}' > /tmp/policy.json
  mc admin policy rm gentian "${APP_ACCESS_KEY}-policy" >/dev/null 2>&1 || true
  mc admin policy create gentian "${APP_ACCESS_KEY}-policy" /tmp/policy.json
  mc admin policy attach gentian "${APP_ACCESS_KEY}-policy" --user "${APP_ACCESS_KEY}"
  echo "minio user for bucket %[1]s ready"
fi
echo "bucket %[1]s ready"`, bucket)
}

// --- destroying a store -----------------------------------------------------

// The scripts below run in the store's own namespace with its admin
// credential. Each ends in success only when what it was asked to remove is
// verifiably not there: no command's failure is discarded, and "already
// absent" is established by asking the store, never inferred from a failed
// delete. They are the only scripts that destroy a store, for one app or for
// a whole tenant.

// Names of the destroy Jobs. One per store and app, in the store's
// namespace; the tenant's label and the app's are on each.
func PostgresDestroyJobName(tenantName, app string) string {
	return fmt.Sprintf("pg-delete-%s-%s", tenantName, app)
}

func MariaDBDestroyJobName(tenantName, app string) string {
	return fmt.Sprintf("mariadb-delete-%s-%s", tenantName, app)
}

func ObjectStorageDestroyJobName(tenantName, app string) string {
	return fmt.Sprintf("s3-delete-%s-%s", tenantName, app)
}

func CacheDestroyJobName(tenantName, app string) string {
	return fmt.Sprintf("redis-acl-delete-%s-%s", tenantName, app)
}

// Which databases are an app's.
//
// One rule, for the three acts that need it: an export copies them, a
// restore puts them back and a purge drops them.
//
// The provisioned database is the app's (DatabaseName). On PostgreSQL so is
// every other database the app's role owns: a role may create databases only
// when its profile asks (allowDynamicDatabaseCreation grants CREATEDB), what
// it creates it owns, and the server records the owner -- so ownership is
// both exact and something the server can be asked. postgresOwnedSQL is that
// question; it leaves out the provisioned database, which every act handles
// by name.
//
// On MariaDB a database has no owner, and the rule is by name: the
// provisioned database, and every database named with it as a prefix that no
// other account holds rights on. mariadb.go states it and holds its one
// query (mariadbOwnedSQL); the same names are all the app's user is granted.
const postgresOwnedSQL = `SELECT d.datname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba
 WHERE r.rolname = :'app_role' AND d.datname <> :'app_db' AND NOT d.datistemplate
 ORDER BY d.datname;`

// postgresDestroyScript drops an app's database, every database its role
// still owns (postgresOwnedSQL), and the role.
//
// It connects through the admin Secret's host, which is the cluster's
// read-write Service: the primary, whichever instance that is. The owned
// databases are read into a file first and read back line by line: a
// command substitution in a for list does not stop the script when it fails,
// and it splits a name with a space in it into two names that are not there.
// Every name is passed to psql as a variable and quoted by the server.
func postgresDestroyScript(database, role string) string {
	return fmt.Sprintf(`set -euo pipefail
ROLE=%[4]s
DB=%[5]s
psql -v ON_ERROR_STOP=1 -tA -v app_role="${ROLE}" -v app_db="${DB}" -d postgres > /tmp/owned <<'PSQL'
%[3]s
PSQL
printf '%%s\n' "${DB}" >> /tmp/owned
while IFS= read -r db; do
  [ -n "${db}" ] || continue
  psql -v ON_ERROR_STOP=1 -v db="${db}" -d postgres >/dev/null <<'PSQL'
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = :'db';
SELECT format('DROP DATABASE IF EXISTS %%I', :'db')
\gexec
PSQL
  echo "database ${db} dropped"
done < /tmp/owned
psql -v ON_ERROR_STOP=1 -c "DO \$\$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[2]s') THEN EXECUTE 'DROP OWNED BY \"%[2]s\"'; END IF; END \$\$;" postgres
psql -v ON_ERROR_STOP=1 -c "DROP ROLE IF EXISTS \"%[2]s\";" postgres
left="$(psql -v ON_ERROR_STOP=1 -tAc "SELECT count(*) FROM pg_database WHERE datname = '%[1]s'" postgres)"
if [ "${left}" != "0" ]; then
  echo "ERROR: database %[1]s is still there" >&2; exit 1
fi
echo "role %[2]s dropped"
`, database, role, postgresOwnedSQL, shellSingleQuote(role), shellSingleQuote(database))
}

// objectStorageDestroyScript removes the bucket and the user and policy that
// were made for it. The user is found through the policy, whose statement
// names the bucket: the key pair itself was seeded and is not known to a
// delete Job.
//
// Whether the bucket exists is read from the server's own listing, so a
// server that cannot be reached fails the Job. `mc rb … || echo "already
// gone"` reported success for exactly that.
func objectStorageDestroyScript(bucket string) string {
	return fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}" >/dev/null
buckets="$(mc ls gentian)"
if printf '%%s\n' "${buckets}" | grep -q ' %[1]s/$'; then
  mc rb --force "gentian/%[1]s"
  echo "bucket %[1]s removed"
else
  echo "bucket %[1]s is not there"
fi
policies="$(mc admin policy ls gentian)"
for policy in ${policies}; do
  case "${policy}" in *-policy) ;; *) continue ;; esac
  info="$(mc admin policy info gentian "${policy}")"
  case "${info}" in *'arn:aws:s3:::%[1]s"'*) ;; *) continue ;; esac
  user="${policy%%-policy}"
  if ! mc admin user rm gentian "${user}"; then
    if mc admin user info gentian "${user}" >/dev/null 2>&1; then
      echo "ERROR: user ${user} could not be removed" >&2; exit 1
    fi
    echo "user ${user} is not there"
  fi
  mc admin policy rm gentian "${policy}"
  echo "user ${user} and its policy removed"
done
echo "object storage for %[1]s purged"
`, bucket)
}

// cacheDestroyScript removes the app's ACL user.
//
// redis-cli exits 0 when the server answers with an error, so the exit code
// says nothing. The script asks for the user list afterwards and passes only
// if the list could be read -- it always names the default user -- and the
// app's user is not in it.
//
// The user is all that is removed. The keys the app wrote stay in the shared
// instance: they carry no owner, and nothing here can tell them from another
// app's.
func cacheDestroyScript(user string) string {
	return fmt.Sprintf(`set -eu
cli() { redis-cli -h "$REDIS_HOST" -p "${REDIS_PORT:-6379}" -a "$REDIS_PASSWORD" --no-auth-warning "$@"; }
cli ACL DELUSER '%[1]s'
users="$(cli ACL LIST)"
case "${users}" in
  *"user default "*) ;;
  *) echo "ERROR: the cache did not list its users: ${users}" >&2; exit 1 ;;
esac
if printf '%%s\n' "${users}" | grep -q '^user %[1]s '; then
  echo "ERROR: cache user %[1]s is still there" >&2; exit 1
fi
echo "cache user %[1]s is gone"
`, user)
}

// DestroyDeadline bounds one destroy Job. The purge of one app answers a
// person who is waiting and gives each Job a short one; the deletion of a
// tenant is not waited for by anybody and gives a bucket the time it takes
// to empty.
type DestroyDeadline int64

const (
	// DestroyWithinARequest is for a Job a request is waiting on.
	DestroyWithinARequest DestroyDeadline = 100
	// DestroyInTheBackground is for a Job a reconciler comes back to.
	DestroyInTheBackground DestroyDeadline = DestroyDeadline(meta.ProvisioningJobActiveDeadlineSeconds)
)

// PostgresDestroyJob drops an app's PostgreSQL database and role.
func PostgresDestroyJob(tenant *gentianov1alpha1.Tenant, app string, deadline DestroyDeadline) *batchv1.Job {
	return destroyJob(layout.System("postgresql"), PostgresDestroyJobName(tenant.Name, app), tenant.Name, app, deadline,
		corev1.Container{
			Name:    "delete-db",
			Image:   kernel.PostgresProvisionerImage(),
			Command: []string{"/bin/bash", "-c", postgresDestroyScript(DatabaseName(tenant, app), PostgresRole(tenant.Name, app))},
			Env:     PostgresAdminEnv(),
		})
}

// MariaDBDestroyJob drops an app's MariaDB databases -- the provisioned one
// and every one that is the app's by name -- and its user.
func MariaDBDestroyJob(tenant *gentianov1alpha1.Tenant, app string, deadline DestroyDeadline) *batchv1.Job {
	return destroyJob(layout.System("mariadb"), MariaDBDestroyJobName(tenant.Name, app), tenant.Name, app, deadline,
		corev1.Container{
			Name:    "delete-db",
			Image:   kernel.MariaDBProvisionerImage(),
			Command: []string{"/bin/bash", "-c", mariadbDestroyScript(DatabaseName(tenant, app), MariaDBUser(tenant.Name, app))},
			Env:     MariaDBAdminEnv(),
		})
}

// ObjectStorageDestroyJob removes a bucket with its user and policy. unit is
// the app the bucket is for, or the name of a bucket that is no app's (the
// tenant's backup bucket) -- S3Bucket and BackupBucket agree on what a unit
// names, which is what lets one Job serve both.
func ObjectStorageDestroyJob(tenant *gentianov1alpha1.Tenant, unit string, deadline DestroyDeadline) *batchv1.Job {
	return destroyJob(layout.System("s3"), ObjectStorageDestroyJobName(tenant.Name, unit), tenant.Name, unit, deadline,
		corev1.Container{
			Name:    "delete-bucket",
			Image:   mcImage,
			Command: []string{"/bin/sh", "-c", objectStorageDestroyScript(S3Bucket(tenant, unit))},
			Env:     PlatformStorageEnv(),
		})
}

// CacheDestroyJob removes an app's user from the shared cache.
func CacheDestroyJob(tenant *gentianov1alpha1.Tenant, app string, deadline DestroyDeadline) *batchv1.Job {
	return destroyJob(layout.System("cache"), CacheDestroyJobName(tenant.Name, app), tenant.Name, app, deadline,
		corev1.Container{
			Name:    "del-acl-user",
			Image:   kernel.RedisProvisionerImage(),
			Command: []string{"/bin/sh", "-c", cacheDestroyScript(RedisACLUser(tenant.Name, app))},
			Env:     CacheAdminEnv(),
		})
}

// RedisAdminSecret holds the shared cache's admin credential, in its
// namespace.
const RedisAdminSecret = "redis-admin"

// destroyJob wraps one destroy script as a Job.
//
// It does not retry inside itself: one more pod after a failure and then the
// Job is Failed, with a deadline over both. A destroy Job that cannot do its
// work has to say so while somebody is still looking; whoever runs it
// decides whether to run it again. The containers run under the restricted
// security context the store namespaces enforce -- without it the pod is
// refused admission and the Job sits there with nothing behind it.
func destroyJob(namespace, name, tenantName, app string, deadline DestroyDeadline, container corev1.Container) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	seconds := int64(deadline)
	backoff := int32(1)
	no := false
	container.SecurityContext = &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				meta.TenantLabel:    tenantName,
				meta.ManagedByLabel: meta.ManagedByValue,
				meta.AppLabel:       app,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &seconds,
			BackoffLimit:            &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
}

// StoreDestroyJobs are the Jobs that destroy the kernel stores one app
// owns, each under its kind, for the kinds the app's profile declares.
func StoreDestroyJobs(tenant *gentianov1alpha1.Tenant, app string, stores Stores, deadline DestroyDeadline) map[Kind]*batchv1.Job {
	jobs := map[Kind]*batchv1.Job{}
	switch stores.Database {
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		jobs[KindDatabase] = PostgresDestroyJob(tenant, app, deadline)
	case gentianov1alpha1.DatabaseEngineMariaDB:
		jobs[KindDatabase] = MariaDBDestroyJob(tenant, app, deadline)
	}
	if stores.S3 {
		jobs[KindObjectStorage] = ObjectStorageDestroyJob(tenant, app, deadline)
	}
	if stores.Redis {
		jobs[KindCache] = CacheDestroyJob(tenant, app, deadline)
	}
	return jobs
}
