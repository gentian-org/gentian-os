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

package netpolicy

import (
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// BuildInput collects tenant state for MAC policy generation.
type BuildInput struct {
	TenantName    string
	Namespace     string
	Apps          []gentianov1alpha1.TenantApp
	Profiles      map[string]*gentianov1alpha1.AppProfile
	Bindings      []*gentianov1alpha1.IntegrationBinding
	Grants        map[string]*gentianov1alpha1.AppGrant
	Config        Config
	KubeAPIEndpts *discoveryv1.EndpointSlice
}

// BuildDesired returns all operator-managed NetworkPolicies for a tenant.
func BuildDesired(in BuildInput) []*networkingv1.NetworkPolicy {
	out := []*networkingv1.NetworkPolicy{
		BaselineNetworkPolicy(in.TenantName, in.Namespace, in.Config, in.KubeAPIEndpts),
	}
	if np := AppInitAccessNetworkPolicy(in.TenantName, in.Namespace, in.Config); np != nil {
		out = append(out, np)
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
	}

	for _, binding := range in.Bindings {
		var grant *gentianov1alpha1.AppGrant
		if in.Grants != nil {
			grant = in.Grants[binding.Spec.Consumer.App]
		}
		if np := ContractAllowNetworkPolicy(in.TenantName, binding, grant); np != nil {
			out = append(out, np)
		}
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
		if profile == nil || profile.Spec.KernelRequirements == nil || profile.Spec.KernelRequirements.Cache == nil {
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

// ManagedPolicyNames returns the set of operator-owned policy names (excluding baseline).
func ManagedPolicyNames(in BuildInput) map[string]struct{} {
	names := map[string]struct{}{
		baselinePolicyName: {},
		appInitPolicyName:  {},
	}
	for _, app := range in.Apps {
		names[kernelPolicyName(app.Profile)] = struct{}{}
		names[appInternalPolicyName(app.Profile)] = struct{}{}
	}
	for _, binding := range in.Bindings {
		names[contractPolicyName(binding.Name)] = struct{}{}
	}
	names[tenantCacheEgressPolicyName()] = struct{}{}
	names[tenantCacheIngressPolicyName()] = struct{}{}
	return names
}
