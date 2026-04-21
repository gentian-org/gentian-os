/*
Copyright 2026 The Gentian Authors.

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

// Package secrets provides the shared "seed OpenBao from the kernel" primitive
// used by every Tenant reconciler (identity, ldap, database, mariadb, storage,
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
// expected by the app-workspace and service-specific Tofu modules.
//
// The package intentionally exposes a narrow surface — one method per
// kernel-requirement category — so every reconciler performs the same
// "derive → write-once → return derived struct → pass to provisioning Job
// via env var" sequence with zero duplication.
package secrets
