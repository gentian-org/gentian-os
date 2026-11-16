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

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// signInFramers is who may put the identity provider's pages in a frame,
// besides those pages themselves: the platform's desktop, every tenant's
// desktop, and the hosts of every component that has more than one.
//
// A desktop opens an app in a frame, and the app's sign-in runs in that
// frame, so the desktop is the page above the sign-in. A component on several
// hosts may frame one of them in another (a file store and the document
// editor it opens), and each host keeps a session of its own, so the inner
// host's first sign-in runs inside the outer host's frame -- both are above
// the sign-in then, and a browser checks every page above it. No other app's
// page is above a sign-in, and none is named: a page that may frame the
// sign-in can lay something of its own over it.
//
// The kernel route builder and the identity provider's route reconciler both
// write this list onto the same route, so both take it from here.
func signInFramers(ctx context.Context, c client.Reader, tenants []gentianov1alpha1.Tenant, kernelDomain, tenancyMode, kernelRealm string) ([]string, error) {
	if kernelDomain == "" {
		return nil, nil
	}
	seen := map[string]bool{"": true}
	var hosts []string
	add := func(h string) {
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	add(platformDesktopHost(kernelDomain))
	profiles := map[string]*gentianov1alpha1.ComponentProfile{}
	for i := range tenants {
		tenant := &tenants[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		zone := zoneNamesOf(tenant, kernelDomain, tenancyMode, kernelRealm)
		add(zoneDesktopHost(zone))
		comps := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, comps, client.InNamespace(tenant.NamespaceName())); err != nil {
			return nil, fmt.Errorf("list components of tenant %s: %w", tenant.Name, err)
		}
		sort.Slice(comps.Items, func(a, b int) bool { return comps.Items[a].Name < comps.Items[b].Name })
		for j := range comps.Items {
			comp := &comps.Items[j]
			if comp.DeletionTimestamp != nil {
				continue
			}
			profile, err := cachedComponentProfile(ctx, c, profiles, comp.Spec.ProfileRef.Name)
			if err != nil {
				return nil, err
			}
			if own := componentOwnHosts(zone, comp, profile); len(own) > 1 {
				for _, h := range own {
					add(h)
				}
			}
		}
	}
	return hosts, nil
}

// cachedComponentProfile is the named profile, read once per walk; nil when
// there is none.
func cachedComponentProfile(
	ctx context.Context,
	c client.Reader,
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
		return nil, fmt.Errorf("get ComponentProfile %s: %w", name, err)
	}
	cache[name] = profile
	return profile, nil
}
