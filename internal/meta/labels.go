/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package meta holds shared Kubernetes label keys and values used by the
// operator and tenant shell helpers.
package meta

const (
	TenantLabel    = "gentianos.io/tenant"
	AppLabel       = "gentianos.io/app"
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "gentian-os"

	// NetPolicyTypeLabel classifies operator-managed NetworkPolicies.
	NetPolicyTypeLabel    = "gentianos.io/netpolicy-type"
	NetPolicyBaseline     = "baseline"
	NetPolicyKernel       = "kernel-access"
	NetPolicyAppInternal  = "app-internal-access"
	NetPolicyAppEgress    = "app-egress"
	NetPolicyContract     = "contract-allow"
	NetPolicyTenantCache  = "tenant-cache-access"
	NetPolicyTenantExport = "tenant-export"

	// ComponentLabel classifies pods within a tenant app (init jobs, sidecars, etc.).
	ComponentLabel = "gentianos.io/component"
	// TenantCacheComponentValue marks the shared per-tenant Memcached workload.
	TenantCacheComponentValue = "tenant-cache"
)
