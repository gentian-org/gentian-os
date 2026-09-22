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

// Package layout is the cluster's namespace layout as Go code reads it: the
// same facts as kernel/namespaces.yaml, which the installer reads. The lint
// verify-namespace-layout fails when the two disagree.
//
// Code selects namespaces by label and asks for kernel namespaces by function;
// it never spells a kernel namespace's name.
package layout

// Label keys every namespace carries.
const (
	LabelTier     = "gentianos.io/tier"
	LabelFunction = "gentianos.io/function"
	LabelTenant   = "gentianos.io/tenant"
)

// Tier is a namespace's place in the trust model.
type Tier string

const (
	TierKernel    Tier = "kernel"
	TierSystem    Tier = "system"
	TierSystemDMZ Tier = "system-dmz"
	TierShared    Tier = "shared"
	TierTenant    Tier = "tenant"
	TierTenantDMZ Tier = "tenant-dmz"
)

// Function names what a kernel namespace is for.
type Function string

const (
	GitOps         Function = "gitops"
	Provisioning   Function = "provisioning"
	Secrets        Function = "secrets"
	Seal           Function = "seal"
	Data           Function = "data"
	Authentication Function = "authentication"
	Authorization  Function = "authorization"
	Control        Function = "control"
	Edge           Function = "edge"
	Admission      Function = "admission"
)

// kernel maps a function to its namespace. Order is creation order.
var kernel = []struct {
	fn   Function
	name string
}{
	{GitOps, "kernel-gitops"},
	{Provisioning, "kernel-provisioning"},
	{Secrets, "kernel-secrets"},
	{Seal, "kernel-seal"},
	{Data, "kernel-data"},
	{Authentication, "kernel-authentication"},
	{Authorization, "kernel-authorization"},
	{Control, "kernel-control"},
	{Edge, "kernel-edge"},
	{Admission, "kernel-admission"},
}

// Kernel returns the namespace with a kernel function. It panics on a function
// that does not exist: that is a programming error, not a runtime condition.
func Kernel(fn Function) string {
	for _, k := range kernel {
		if k.fn == fn {
			return k.name
		}
	}
	panic("layout: no kernel namespace has function " + string(fn))
}

// KernelNamespaces returns every kernel namespace, in creation order.
func KernelNamespaces() []string {
	out := make([]string, len(kernel))
	for i, k := range kernel {
		out[i] = k.name
	}
	return out
}

// Tenant returns a tenant's namespace.
func Tenant(name string) string { return "tenant-" + name }

// TenantDMZ returns a tenant's perimeter namespace.
func TenantDMZ(name string) string { return "tenant-" + name + "-dmz" }

// Shared returns a shared app's namespace.
func Shared(app string) string { return "shared-" + app }

// System returns a system function's namespace.
func System(fn string) string { return "system-" + fn }

// Labels returns the labels a namespace of the given tier and function carries.
func Labels(tier Tier, fn string) map[string]string {
	return map[string]string{LabelTier: string(tier), LabelFunction: fn}
}

// TenantLabels returns the labels of a tenant namespace, or its DMZ.
func TenantLabels(tenant string, dmz bool) map[string]string {
	tier := TierTenant
	if dmz {
		tier = TierTenantDMZ
	}
	return map[string]string{LabelTier: string(tier), LabelFunction: "tenant", LabelTenant: tenant}
}

// MaxTenantName bounds a tenant name so that tenant-<t>-dmz is a valid
// namespace name (63 characters).
const MaxTenantName = 63 - len("tenant-") - len("-dmz")
