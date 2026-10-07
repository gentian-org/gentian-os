/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import "strings"

const (
	// TenancyModeMulti is the default: the platform tenant plus any number of
	// user tenants, each at {label}.{tenant}.{kernelDomain} unless a
	// TenantDomain binds a custom domain.
	TenancyModeMulti = "multi"

	// TenancyModeSingle is a cluster for one organisation: the platform
	// tenant plus exactly one user tenant, named SingleUserTenantName, which
	// lives on the cluster's own addresses -- {label}.{kernelDomain}, with no
	// tenant name in between. A second user tenant is refused.
	TenancyModeSingle = "single"

	// PlatformTenantName is the tenant every install scaffolds for the
	// cluster's administrators: the one that adopts the kernel realm. It is
	// in every cluster under either mode and is never counted as a user
	// tenant. Its desktop is {PlatformTenantName}.{kernelDomain} and its
	// other components one label below that.
	PlatformTenantName = "platform"

	// SingleUserTenantName is the one user tenant of a single-tenancy
	// cluster. Under TenancyModeMulti it is an ordinary tenant name.
	SingleUserTenantName = "user"

	// SingleUserAdminLocalPart is the local part of the administrator of a
	// tenant whose domain is the cluster's own. The platform administrator is
	// admin@{kernelDomain} in the kernel realm, and its password derivation
	// and recovery depend on that name; the user tenant's administrator has
	// an address on the same domain and must not be the same one.
	SingleUserAdminLocalPart = "user-admin"
)

// NormalizeTenancyMode returns TenancyModeSingle or TenancyModeMulti.
func NormalizeTenancyMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), TenancyModeSingle) {
		return TenancyModeSingle
	}
	return TenancyModeMulti
}

// OnClusterDomain reports whether this tenant's base domain is the cluster's
// own under the given mode: the one user tenant of a single-tenancy cluster,
// and nobody else. The platform tenant is never it, whatever the mode.
func (t *Tenant) OnClusterDomain(tenancyMode string) bool {
	return t != nil && t.Name == SingleUserTenantName &&
		NormalizeTenancyMode(tenancyMode) == TenancyModeSingle
}

// NamespaceName is the namespace a tenant's workloads run in.
//
// On the type rather than in a controller helper because the usage sampler and
// the resources API both need it and neither may import the controller package.
// A second copy would be a second place for the isolation override to be
// forgotten, and a sampler reading tenant-<name> while the workloads run
// somewhere else records an empty namespace as an idle one.
func (t *Tenant) NamespaceName() string {
	if t == nil {
		return ""
	}
	if t.Spec.Isolation != nil && t.Spec.Isolation.Namespace != "" {
		return t.Spec.Isolation.Namespace
	}
	return "tenant-" + t.Name
}
