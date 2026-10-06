/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

import "fmt"

// Mount is the KV v2 mount that backs every gentian-os secret.
const Mount = "secret"

// CategoryPath returns the canonical KV v2 logical path (no "data/" prefix) for
// a kernel-requirement category (oidc, database, s3, cache, smtp, imap).
//
// The resulting path is read via ExternalSecret by Crossplane-managed app compositions.
func CategoryPath(tenant, app, category string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps/%s/%s", tenant, app, category)
}

// AppPath is everything stored for one app of a tenant: each category the
// kernel provisions for it and each secret generated for it is below this. An
// extension of an app has a subtree of its own beside it, keyed
// "{app}-{extension}". Purging an app's credentials is deleting these.
func AppPath(tenant, app string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps/%s", tenant, app)
}

// AppsPath is where a tenant's per-app subtrees are: listing it names every
// app, installed or not, that still has something stored.
func AppsPath(tenant string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps", tenant)
}

// InternalPath returns the canonical KV v2 logical path for a per-app internal
// secret (an AppProfile.spec.appSecrets entry). The value is stored with a
// single "value" key so the ExternalSecret can read it generically.
func InternalPath(tenant, app, name string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps/%s/internal/%s", tenant, app, name)
}

// ContractPath returns the canonical KV v2 logical path for a contract integration
// secret shared between provider and consumer.
func ContractPath(tenant, contract string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/contracts/%s", tenant, contract)
}

// KernelPath returns the canonical KV v2 logical path for a kernel-level
// secret that is shared across all tenants (e.g. the master password, SMTP
// relay credentials, OCI registry pull secrets). Kernel paths intentionally
// omit any tenant component.
func KernelPath(category, name string) string {
	return fmt.Sprintf("gentian-os/kernel/%s/%s", category, name)
}

// MasterPasswordPath is the canonical kernel path where seed-openbao.sh
// writes the platform master password. The operator reads it once at
// startup and feeds it to the Deriver.
const MasterPasswordPath = "gentian-os/kernel/internal/master-password"

// TenantPath is everything this platform stores for one tenant. Every other
// tenant path is built below it, which is what makes purging a tenant a single
// subtree rather than a list that has to be kept in step with the paths.
func TenantPath(tenant string) string {
	return fmt.Sprintf("gentian-os/tenants/%s", tenant)
}

// TenantAdminPath returns the canonical KV v2 logical path for the
// tenant-scoped realm admin credentials created by the identity reconciler.
func TenantAdminPath(tenant string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/admin", tenant)
}
