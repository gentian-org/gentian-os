/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing the License.
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
	NetPolicyTypeLabel = "gentianos.io/netpolicy-type"
	NetPolicyBaseline  = "baseline"
	NetPolicyKernel    = "kernel-access"
	NetPolicyAppInit     = "app-init-access"
	NetPolicyAppInternal = "app-internal-access"
	NetPolicyContract    = "contract-allow"
	NetPolicyTenantCache = "tenant-cache-access"

	// ComponentLabel classifies pods within a tenant app (init jobs, sidecars, etc.).
	ComponentLabel = "gentianos.io/component"
	// AppInitComponentValue marks bootstrap Jobs (db-init, s3-init) that need OpenBao + kernel egress.
	AppInitComponentValue = "app-init"
	// TenantCacheComponentValue marks the shared per-tenant Memcached workload.
	TenantCacheComponentValue = "tenant-cache"

	// portalRedirectComponent marks portal redirect resources owned by the shared kernel portal.
	PortalRedirectComponentLabel = ComponentLabel
	PortalRedirectComponentValue = "portal-redirect"

	EnvoyGatewayInstallNamespace = "envoy-gateway-system"
)
