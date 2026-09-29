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

package netpolicy

import (
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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
	Privileges    []gentianov1alpha1.TenantPrivilegeGrant
	Bindings      []*gentianov1alpha1.IntegrationBinding
	Grants        map[string]*gentianov1alpha1.AppGrant
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
		if profile == nil || profile.Services() == nil || profile.Services().Cache == nil {
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
		exportPolicyName(): {},
	}
	for _, app := range in.Apps {
		names[kernelPolicyName(app.Profile)] = struct{}{}
		names[appInternalPolicyName(app.Profile)] = struct{}{}

		if len(in.grantedEgress(app.Profile, in.Profiles[app.Profile])) > 0 {
			names[appEgressPolicyName(app.Profile)] = struct{}{}
		}
	}
	for _, binding := range in.Bindings {
		names[contractPolicyName(binding.Name)] = struct{}{}
	}
	names[tenantCacheEgressPolicyName()] = struct{}{}
	names[tenantCacheIngressPolicyName()] = struct{}{}
	return names
}
