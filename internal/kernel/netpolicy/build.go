/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/security"
)

// grantedEgress are the egress rules for one install that the tenant's grants
// answer. A declared privilege nobody granted opens nothing, and the two
// callers below must agree on that or the policy would be built and then not
// listed, or listed and not built.
func (in BuildInput) grantedEgress(install string, profile *gentianov1alpha1.ComponentProfile) []networkingv1.NetworkPolicyEgressRule {
	if profile == nil {
		return nil
	}
	granted := security.GrantedSet(security.TenantGrants(
		&gentianov1alpha1.Tenant{Spec: gentianov1alpha1.TenantSpec{Privileges: in.Privileges}},
		install), time.Now())
	return security.EgressRulesFor(profile, granted)
}

// BuildInput collects tenant state for MAC policy generation.
type BuildInput struct {
	TenantName string
	Namespace  string
	Apps       []gentianov1alpha1.TenantApp
	Profiles   map[string]*gentianov1alpha1.ComponentProfile
	// Privileges are the grants the Tenant carries. Egress beyond the
	// baseline is a privilege somebody has to have answered (AD-5), and this
	// is where the answer is: an app installed through Tenant.spec.apps has
	// no Component to hang a grant on.
	Privileges []gentianov1alpha1.TenantPrivilegeGrant
	Bindings   []*gentianov1alpha1.IntegrationBinding
	Grants     map[string]*gentianov1alpha1.AppGrant
	// PodSelectors are the labels each app's pods carry, for an app whose
	// delivery does not put the app label on them. A contract's two sides
	// are selected by these.
	PodSelectors  map[string]map[string]string
	Config        Config
	KubeAPIEndpts *discoveryv1.EndpointSlice
}

// BuildDesired returns all operator-managed NetworkPolicies for a tenant.
func BuildDesired(in BuildInput) []*networkingv1.NetworkPolicy {
	out := []*networkingv1.NetworkPolicy{
		BaselineNetworkPolicy(in.TenantName, in.Namespace, in.Config, in.KubeAPIEndpts),
		ExportJobNetworkPolicy(in.TenantName, in.Namespace, in.Config),
	}

	for _, app := range in.Apps {
		profile := in.Profiles[app.Profile]
		if profile == nil {
			continue
		}
		if np := KernelAccessNetworkPolicy(in.TenantName, in.Namespace, app.Profile, profile, in.Config); np != nil {
			out = append(out, np)
		}
		out = append(out, AppInternalAccessNetworkPolicy(in.TenantName, in.Namespace, app.Profile))
		// Only egress somebody GRANTED. This used to apply whatever the
		// profile declared, to every tenant that installed it, with nobody
		// asked -- which is the hole AD-5 exists to close.
		if rules := in.grantedEgress(app.Profile, profile); len(rules) > 0 {
			out = append(out, AppEgressNetworkPolicy(in.TenantName, in.Namespace, app.Profile, rules))
		}
	}

	for _, binding := range in.Bindings {
		var grant *gentianov1alpha1.AppGrant
		if in.Grants != nil {
			grant = in.Grants[binding.Spec.Consumer.App]
		}
		out = append(out, ContractNetworkPolicies(in.TenantName, binding, grant, in.PodSelectors)...)
	}

	cacheApps := cacheAppNames(in)
	if np := TenantCacheEgressNetworkPolicy(in.TenantName, in.Namespace, cacheApps); np != nil {
		out = append(out, np)
	}
	if np := TenantCacheIngressNetworkPolicy(in.TenantName, in.Namespace, cacheApps); np != nil {
		out = append(out, np)
	}
	return out
}

func cacheAppNames(in BuildInput) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, app := range in.Apps {
		profile := in.Profiles[app.Profile]
		// Memcached only: it is the cache that lives in the tenant's own
		// namespace. A Redis app has no business with it.
		if provisioner.CacheEngineOf(profile) != gentianov1alpha1.CacheEngineMemcached {
			continue
		}
		if _, ok := seen[app.Profile]; ok {
			continue
		}
		seen[app.Profile] = struct{}{}
		names = append(names, app.Profile)
	}
	return names
}

// anyProfileUnknown reports whether an installed app's profile could not be
// read, so that what it declares is not known.
func (in BuildInput) anyProfileUnknown() bool {
	for _, app := range in.Apps {
		if in.Profiles[app.Profile] == nil {
			return true
		}
	}
	return false
}

// ManagedPolicyNames returns the set of operator-owned policy names (excluding baseline).
func ManagedPolicyNames(in BuildInput) map[string]struct{} {
	names := map[string]struct{}{
		baselinePolicyName: {},
		exportPolicyName(): {},
	}
	for _, app := range in.Apps {
		// Kept while it is built, and while the profile cannot be read:
		// what an unknown profile opens is unknown, and its app keeps what
		// it had. A profile that is known to open nothing here loses the
		// policy that opened what it used to.
		profile := in.Profiles[app.Profile]
		if profile == nil || KernelAccessNetworkPolicy(in.TenantName, in.Namespace, app.Profile, profile, in.Config) != nil {
			names[kernelPolicyName(app.Profile)] = struct{}{}
		}
		names[appInternalPolicyName(app.Profile)] = struct{}{}

		if len(in.grantedEgress(app.Profile, in.Profiles[app.Profile])) > 0 {
			names[appEgressPolicyName(app.Profile)] = struct{}{}
		}
	}
	// Only the policies of a contract somebody granted: one that lost its
	// grant loses its policies, and with them the way between the two apps.
	for _, binding := range in.Bindings {
		var grant *gentianov1alpha1.AppGrant
		if in.Grants != nil {
			grant = in.Grants[binding.Spec.Consumer.App]
		}
		for _, np := range ContractNetworkPolicies(in.TenantName, binding, grant, in.PodSelectors) {
			names[np.Name] = struct{}{}
		}
	}
	// Only while there is a Memcached app to build them for: named
	// unconditionally they were never removed, and a tenant whose last such
	// app had gone kept the policies that app had needed. As above, an app
	// whose profile cannot be read keeps what it had.
	if len(cacheAppNames(in)) > 0 || in.anyProfileUnknown() {
		names[tenantCacheEgressPolicyName()] = struct{}{}
		names[tenantCacheIngressPolicyName()] = struct{}{}
	}
	return names
}
