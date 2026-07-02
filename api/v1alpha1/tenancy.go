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
