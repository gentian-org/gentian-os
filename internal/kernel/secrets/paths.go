/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package secrets

import "fmt"

// Mount is the KV v2 mount that backs every gentian-os secret.
const Mount = "secret"

// CategoryPath returns the canonical KV v2 logical path (no "data/" prefix) for
// a kernel-requirement category (oidc, database, s3, cache, smtp, imap, ldap).
//
// The resulting path is read via ExternalSecret by Crossplane-managed app compositions.
func CategoryPath(tenant, app, category string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps/%s/%s", tenant, app, category)
}

// InternalPath returns the canonical KV v2 logical path for a per-app internal
// secret (an AppProfile.spec.appSecrets entry). The value is stored with a
// single "value" key so the ExternalSecret can read it generically.
func InternalPath(tenant, app, name string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps/%s/internal/%s", tenant, app, name)
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

// TenantAdminPath returns the canonical KV v2 logical path for the
// tenant-scoped realm admin credentials created by the identity reconciler.
func TenantAdminPath(tenant string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/admin", tenant)
}
