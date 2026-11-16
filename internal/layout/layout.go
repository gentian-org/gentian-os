/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	// TierTenantRuns is a tenant's namespace for runs: pods the dispatcher
	// starts for one call each, and nothing else.
	TierTenantRuns Tier = "tenant-runs"
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
	Observability  Function = "observability"
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
	{Observability, "kernel-observability"},
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

// TenantRuns returns a tenant's namespace for runs.
func TenantRuns(name string) string { return "tenant-" + name + "-runs" }

// System returns a system function's namespace.
func System(fn string) string { return "system-" + fn }

// TenantLabels returns the labels of a tenant namespace, or its DMZ.
func TenantLabels(tenant string, dmz bool) map[string]string {
	tier := TierTenant
	if dmz {
		tier = TierTenantDMZ
	}
	return map[string]string{LabelTier: string(tier), LabelFunction: "tenant", LabelTenant: tenant}
}

// MaxTenantName bounds a tenant name so that tenant-<t>-dmz and
// tenant-<t>-runs, the longer of the two, are valid namespace names (63
// characters).
const MaxTenantName = 63 - len("tenant-") - len("-runs")
