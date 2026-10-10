/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

// The stores an ExternalSecret reads OpenBao through. Both are
// ClusterSecretStores, and each says in its conditions which namespaces may
// name it; ESO refuses an ExternalSecret anywhere else.

// StoreKind is the kind of both stores.
const StoreKind = "ClusterSecretStore"

// KernelStore is the kernel's store, which the Cluster Composition makes. Its
// role in OpenBao reads every kernel path and every tenant's. It is usable
// from the kernel and system namespaces, and from the platform tenant's for
// the one kernel credential that tenant's desktop is given.
const KernelStore = "openbao"

// TenantStore is a tenant's own store, which the Tenant Composition makes. Its
// role reads that tenant's paths and nothing of the kernel or of another
// tenant, and only that tenant's namespace may name it. Every ExternalSecret
// in a tenant's namespace reads through it.
func TenantStore(tenant string) string { return "openbao-tenant-" + tenant }

// StoreRef is the secretStoreRef of an ExternalSecret that reads through the
// store of that name.
func StoreRef(store string) map[string]interface{} {
	return map[string]interface{}{"name": store, "kind": StoreKind}
}
