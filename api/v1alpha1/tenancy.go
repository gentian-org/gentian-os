/*
Copyright 2026 Gentian Organization.

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

package v1alpha1

import "strings"

const (
	// TenancyModeMulti is the default: many tenants on one cluster; app URLs use
	// {sub}.{tenant}.{kernelDomain} unless spec.domain is set.
	TenancyModeMulti = "multi"

	// TenancyModeSingle is for dedicated single-tenant clusters: one Tenant CR
	// named "default", flat app URLs on {sub}.{kernelDomain}.
	TenancyModeSingle = "single"

	// SingleTenantName is the required Tenant metadata.name in single-tenancy mode.
	SingleTenantName = "default"
)

// NormalizeTenancyMode returns TenancyModeSingle or TenancyModeMulti.
func NormalizeTenancyMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), TenancyModeSingle) {
		return TenancyModeSingle
	}
	return TenancyModeMulti
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
