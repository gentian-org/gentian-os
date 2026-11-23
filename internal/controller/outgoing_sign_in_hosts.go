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
	"net/url"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/hostnames"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/security"
)

// liaisonProfile is the profile of the component that keeps people's
// sign-ins at services outside the cluster (internal/liaison).
const liaisonProfile = "liaison"

// isLiaison reports the liaison the platform ships. A catalog cannot bring a
// profile of that name (gitops.PlatformProfile), and asking where it came
// from keeps one that did out all the same.
func isLiaison(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil && profile.Name == liaisonProfile && hostnames.PlatformShipped(profile)
}

// signInIssuers are the authorization servers a bridge's ways to the outside
// name, where a person signs in at the service.
func signInIssuers(profile *gentianov1alpha1.ComponentProfile) []string {
	if profile == nil || profile.Spec.Bridge == nil {
		return nil
	}
	var out []string
	for _, w := range profile.Spec.Bridge.Ways {
		if w.Outside != nil && w.Outside.SignIn != nil && w.Outside.SignIn.OAuth != nil {
			out = append(out, w.Outside.SignIn.OAuth.Issuer)
		}
	}
	return out
}

// bridgeHosts are the hosts outside the cluster a component reaches for the
// tenant's bridges, without naming them itself:
//
//   - the liaison reaches the hosts of the authorization servers the bridges
//     name (signIn.oauth.issuer), where their people sign in;
//   - a component that may speak as a person, the caller of the tenant's
//     agent bridges, reaches the hosts granted to each agent bridge that
//     runs nothing of its own (an api bridge to a tool server outside): the
//     bridge is reached where it is, so its way out is its callers'.
//
// Each only for a bridge whose own privileges are all answered. Approving a
// bridge is what opens these ways; nobody names the hosts again, and a
// bridge that goes, or loses a grant, takes its hosts with it.
//
// For any other component there are none.
func bridgeHosts(ctx context.Context, c client.Client, profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	liaison, caller := isLiaison(profile), netpolicy.SpeaksAsPerson(profile)
	if (!liaison && !caller) || comp == nil || tenant == nil {
		return nil, nil
	}
	comps := &gentianov1alpha1.ComponentList{}
	if err := c.List(ctx, comps, client.InNamespace(comp.Namespace)); err != nil {
		return nil, err
	}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{}
	now := time.Now()
	var out []string
	for i := range comps.Items {
		bridge := &comps.Items[i]
		if !bridge.DeletionTimestamp.IsZero() || bridge.Name == comp.Name {
			continue
		}
		name := bridge.Spec.ProfileRef.Name
		bp, read := profiles[name]
		if !read {
			bp = &gentianov1alpha1.ComponentProfile{}
			if err := c.Get(ctx, client.ObjectKey{Name: name}, bp); err != nil {
				if client.IgnoreNotFound(err) != nil {
					return nil, err
				}
				bp = nil
			}
			profiles[name] = bp
		}
		var issuers []string
		if liaison {
			issuers = signInIssuers(bp)
		}
		outsideBridge := caller && netpolicy.IsAgentBridge(bp) && !netpolicy.DeploysPods(bp)
		if len(issuers) == 0 && !outsideBridge {
			continue
		}
		answered, err := answeredEgress(ctx, c, bp, tenant)
		if err != nil {
			return nil, err
		}
		if len(security.PendingPrivilegesUnder(bp, bridge, now, answered)) > 0 {
			continue
		}
		for _, issuer := range issuers {
			// The schema admits https:// and a host only; anything else is
			// left out, never carried.
			if u, err := url.Parse(issuer); err == nil && u.Scheme == "https" && u.Host != "" {
				out = append(out, u.Host)
			}
		}
		if outsideBridge {
			out = append(out, security.GrantedEgressHostsUnder(bp, bridge, now, answered)...)
		}
	}
	return out, nil
}

// liaisonOfBridge wakes the liaison of a bridge's tenant when the bridge
// changes: installed, granted, refused or gone, it changes what the liaison
// reaches.
func liaisonOfBridge(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return liaisonsWokenBy(ctx, c, obj)
	})
}

// liaisonsWokenBy are the components liaisonOfBridge wakes for obj: the
// liaison, for a bridge that names where people sign in, and the callers of
// the tenant's agent bridges, for an agent bridge that runs nothing of its
// own.
func liaisonsWokenBy(ctx context.Context, c client.Client, obj client.Object) []reconcile.Request {
	bridge, ok := obj.(*gentianov1alpha1.Component)
	if !ok || bridge.Spec.ProfileRef.Name == liaisonProfile {
		return nil
	}
	bp := &gentianov1alpha1.ComponentProfile{}
	if err := c.Get(ctx, client.ObjectKey{Name: bridge.Spec.ProfileRef.Name}, bp); err != nil {
		return nil
	}
	signIn := len(signInIssuers(bp)) > 0
	outside := netpolicy.IsAgentBridge(bp) && !netpolicy.DeploysPods(bp)
	if !signIn && !outside {
		return nil
	}
	comps := &gentianov1alpha1.ComponentList{}
	if err := c.List(ctx, comps, client.InNamespace(bridge.Namespace)); err != nil {
		return nil
	}
	callers := map[string]bool{}
	if outside {
		profiles := &gentianov1alpha1.ComponentProfileList{}
		if err := c.List(ctx, profiles); err == nil {
			for i := range profiles.Items {
				if netpolicy.SpeaksAsPerson(&profiles.Items[i]) {
					callers[profiles.Items[i].Name] = true
				}
			}
		}
	}
	var out []reconcile.Request
	for i := range comps.Items {
		name := comps.Items[i].Spec.ProfileRef.Name
		if (signIn && name == liaisonProfile) || callers[name] {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&comps.Items[i])})
		}
	}
	return out
}
