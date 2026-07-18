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


package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// collectOIDCIngressSubdomainsByTenant returns ingress hostname prefixes for every
// installed AppProfile that declares OIDC, keyed by tenant name. Used to build
// the shared id.<kernel> frame-ancestors policy so portal-embedded apps can
// complete OIDC without per-app manual allowlists.
func collectOIDCIngressSubdomainsByTenant(
	ctx context.Context,
	c client.Client,
	tenants []gentianov1alpha1.Tenant,
) (map[string][]string, error) {
	result := make(map[string][]string)
	profileCache := make(map[string]*gentianov1alpha1.AppProfile)

	for i := range tenants {
		tenant := &tenants[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		seen := make(map[string]struct{})
		for _, app := range tenant.Spec.Apps {
			profile, err := cachedAppProfile(ctx, c, profileCache, app.Profile)
			if err != nil {
				return nil, err
			}
			if profile == nil {
				continue
			}
			for _, sub := range oidcIngressSubdomainsFromProfile(profile) {
				if sub == "" {
					continue
				}
				if _, ok := seen[sub]; ok {
					continue
				}
				seen[sub] = struct{}{}
				result[tenant.Name] = append(result[tenant.Name], sub)
			}
		}
		if subs, ok := result[tenant.Name]; ok {
			sort.Strings(subs)
			result[tenant.Name] = subs
		}
	}
	return result, nil
}

func cachedAppProfile(
	ctx context.Context,
	c client.Client,
	cache map[string]*gentianov1alpha1.AppProfile,
	name string,
) (*gentianov1alpha1.AppProfile, error) {
	if name == "" {
		return nil, nil
	}
	if p, ok := cache[name]; ok {
		return p, nil
	}
	profile := &gentianov1alpha1.AppProfile{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, profile); err != nil {
		if errors.IsNotFound(err) {
			cache[name] = nil
			return nil, nil
		}
		return nil, fmt.Errorf("get AppProfile %s: %w", name, err)
	}
	cache[name] = profile
	return profile, nil
}

func oidcIngressSubdomainsFromProfile(profile *gentianov1alpha1.AppProfile) []string {
	if profile == nil || !appProfileDeclaresOIDC(profile) {
		return nil
	}
	var subs []string
	if profile.Spec.Ingress != nil && profile.Spec.Ingress.SubDomain != "" {
		subs = append(subs, profile.Spec.Ingress.SubDomain)
	}
	for _, ing := range profile.Spec.AdditionalIngresses {
		if ing.SubDomain != "" {
			subs = append(subs, ing.SubDomain)
		}
	}
	if profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Identity != nil &&
		profile.Spec.KernelRequirements.Identity.OIDC != nil {
		for _, uri := range profile.Spec.KernelRequirements.Identity.OIDC.RedirectURIs {
			if sub := oidcRedirectURISubdomain(uri); sub != "" {
				subs = append(subs, sub)
			}
		}
	}
	return subs
}

// oidcRedirectURISubdomain returns the hostname prefix from an OIDC redirect URI
// template such as https://matrix.${TENANT_DOMAIN}/_synapse/client/oidc/callback.
func oidcRedirectURISubdomain(uri string) string {
	const prefix = "https://"
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	host := strings.SplitN(strings.TrimPrefix(uri, prefix), "/", 2)[0]
	if host == "" {
		return ""
	}
	label := strings.SplitN(host, ".", 2)[0]
	if label == "" || strings.Contains(label, "$") {
		return ""
	}
	return label
}

func appProfileDeclaresOIDC(profile *gentianov1alpha1.AppProfile) bool {
	if profile.Spec.KernelRequirements != nil &&
		profile.Spec.KernelRequirements.Identity != nil &&
		profile.Spec.KernelRequirements.Identity.OIDC != nil {
		return true
	}
	for _, sidecar := range profile.Spec.Sidecars {
		if sidecar.KernelRequirements != nil &&
			sidecar.KernelRequirements.Identity != nil &&
			sidecar.KernelRequirements.Identity.OIDC != nil {
			return true
		}
	}
	return false
}
