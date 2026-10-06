/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package secrets provides the shared "seed OpenBao from the kernel" primitive
// used by every Tenant reconciler (identity, database, mariadb, storage,
// cache, mail, apps).
//
// A Seeder derives deterministic per-tenant-per-app credentials from a single
// MASTER_PASSWORD via HKDF-SHA256 (RFC 5869), with canonical-path salts so
// tenant-scoped secrets are diversified by tenant while kernel-shared
// secrets use the same value across tenants. Derived values are persisted
// write-once into OpenBao under the canonical path layout
//
//	secret/data/gentian-os/tenants/{tenant}/apps/{app}/{category}
//	secret/data/gentian-os/tenants/{tenant}/apps/{app}/internal/{name}
//
// consumed by the app reconciler (Pattern B Terraform CRs via set_sensitive
// and Pattern A ExternalSecrets via ESO).
//
// The package intentionally exposes a narrow surface — one method per
// kernel-requirement category — so every reconciler performs the same
// "derive → write-once → return derived struct → pass to provisioning Job
// via env var" sequence with zero duplication.
package secrets
