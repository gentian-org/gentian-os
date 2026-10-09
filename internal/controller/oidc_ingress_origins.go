/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
// installed ComponentProfile that declares OIDC, keyed by tenant name. Used to build
// the shared id.<kernel> frame-ancestors policy so portal-embedded apps can
// complete OIDC without per-app manual allowlists.
func collectOIDCIngressSubdomainsByTenant(
	ctx context.Context,
	c client.Client,
	tenants []gentianov1alpha1.Tenant,
) (map[string][]string, error) {
	result := make(map[string][]string)
	profileCache := make(map[string]*gentianov1alpha1.ComponentProfile)

	for i := range tenants {
		tenant := &tenants[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		seen := make(map[string]struct{})
		// Addons alongside apps, for the same reason the ingress collector walks
		// both: an addon may declare a hostname, and a host absent from this
		// list is an origin the app's own OIDC client will refuse. No addon in
		// the catalogue declares one today, so this changes nothing yet -- it
		// keeps the two walks agreeing, rather than leaving one of them to be
		// discovered later by a login that fails on a host nobody allowed.
		for _, app := range tenant.Spec.Apps {
			for _, profileName := range append([]string{app.Profile}, app.Addons...) {
				profile, err := cachedAppProfile(ctx, c, profileCache, profileName)
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
	cache map[string]*gentianov1alpha1.ComponentProfile,
	name string,
) (*gentianov1alpha1.ComponentProfile, error) {
	if name == "" {
		return nil, nil
	}
	if p, ok := cache[name]; ok {
		return p, nil
	}
	profile := &gentianov1alpha1.ComponentProfile{}
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

func oidcIngressSubdomainsFromProfile(profile *gentianov1alpha1.ComponentProfile) []string {
	if profile == nil || !appProfileDeclaresOIDC(profile) {
		return nil
	}
	// Every gateway host this component answers on. All of them, not just the
	// first: a redirect URI that is not registered is a login that fails at
	// the last step, and an additional host is as real a landing place as the
	// component's own.
	var subs []string
	for _, exposure := range profile.GatewayExposures() {
		if exposure.SubDomain != "" {
			subs = append(subs, exposure.SubDomain)
		}
	}
	if profile.Services() != nil &&
		profile.Services().Identity != nil &&
		profile.Services().Identity.OIDC != nil {
		for _, uri := range profile.Services().Identity.OIDC.RedirectURIs {
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

func appProfileDeclaresOIDC(profile *gentianov1alpha1.ComponentProfile) bool {
	if profile.Services() != nil &&
		profile.Services().Identity != nil &&
		profile.Services().Identity.OIDC != nil {
		return true
	}
	for _, sidecar := range profile.Spec.Extensions {
		if sidecar.ServiceRequirements != nil &&
			sidecar.ServiceRequirements.Identity != nil &&
			sidecar.ServiceRequirements.Identity.OIDC != nil {
			return true
		}
	}
	return false
}
