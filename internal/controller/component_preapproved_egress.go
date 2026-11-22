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
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/hostnames"
	"github.com/gentian-org/gentian-os/internal/security"
)

// preApprovedEgress are the egress requests of profile that policy answers
// for an install of tenant: each request by host whose hosts are all in a
// trusted domain, the cluster's (spec.trustedDomains) or the tenant's own
// while it was delegated one. Read
// on every reconcile, so the hold and the network policy agree with the
// policy as it is now.
//
// A profile the platform ships is answered by the platform: all of its
// requests. No catalog brought it and no tenant chose it, so there is nobody
// in a tenant whose question it is; what such a profile reaches is decided
// with the release that ships it.
func (r *ComponentReconciler) preApprovedEgress(ctx context.Context, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (map[string]struct{}, error) {
	return answeredEgress(ctx, r.Client, profile, tenant)
}

// answeredEgress is preApprovedEgress for any reader of the cluster.
func answeredEgress(ctx context.Context, c client.Client, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (map[string]struct{}, error) {
	if profile == nil || profile.Privileges() == nil || len(profile.Privileges().Egress) == 0 {
		return nil, nil
	}
	if hostnames.PlatformShipped(profile) {
		out := map[string]struct{}{}
		for i := range profile.Privileges().Egress {
			// Hosts named at approval are a person's to name, shipped or not.
			if profile.Privileges().Egress[i].HostsAtApproval {
				continue
			}
			out[security.PrivilegeRef(security.PrivilegeEgress, profile.Privileges().Egress[i].Name)] = struct{}{}
		}
		return out, nil
	}
	trusted, err := security.LoadTrustedDomains(ctx, c)
	if err != nil {
		return nil, err
	}
	return security.TrustedEgress(profile, append(append([]string(nil), trusted...), security.TenantTrustedDomains(tenant)...)), nil
}

// mergeAnswered is the union of two sets of answered requests, nil when
// both are empty.
func mergeAnswered(a, b map[string]struct{}) map[string]struct{} {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(a)+len(b))
	for ref := range a {
		out[ref] = struct{}{}
	}
	for ref := range b {
		out[ref] = struct{}{}
	}
	return out
}

// componentsAskingForEgressPrivileges wakes every Component whose profile
// asks for egress when the PlatformSecurityPolicy changes: a domain added to
// its trusted domains lets a waiting install go on, and one removed takes
// its apps' egress away now, not when something else next wakes them.
func componentsAskingForEgressPrivileges(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return egressAskingComponents(ctx, c)
	})
}

// trustedDomainsChanged passes a Tenant update that changes the domains the
// tenant's own list answers: one added, one removed, or the list delegated
// or taken back.
func trustedDomainsChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*gentianov1alpha1.Tenant)
			after, ok2 := e.ObjectNew.(*gentianov1alpha1.Tenant)
			return ok1 && ok2 && !slices.Equal(security.TenantTrustedDomains(before), security.TenantTrustedDomains(after))
		},
	}
}

// componentsOfTenantAskingForEgress wakes the tenant's Components whose
// profile asks for egress: a domain the tenant stops trusting takes its apps'
// egress away now, and one it starts trusting lets a waiting install go on.
func componentsOfTenantAskingForEgress(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		tenant, ok := obj.(*gentianov1alpha1.Tenant)
		if !ok {
			return nil
		}
		return egressAskingComponents(ctx, c, client.InNamespace(tenant.NamespaceName()))
	})
}

// egressAskingComponents are the Components, of the namespace opts name or
// of every one, whose profile asks for egress.
func egressAskingComponents(ctx context.Context, c client.Client, opts ...client.ListOption) []reconcile.Request {
	profiles := &gentianov1alpha1.ComponentProfileList{}
	if err := c.List(ctx, profiles); err != nil {
		return nil
	}
	asking := map[string]struct{}{}
	for i := range profiles.Items {
		if p := profiles.Items[i].Privileges(); p != nil && len(p.Egress) > 0 {
			asking[profiles.Items[i].Name] = struct{}{}
		}
	}
	if len(asking) == 0 {
		return nil
	}
	comps := &gentianov1alpha1.ComponentList{}
	if err := c.List(ctx, comps, opts...); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range comps.Items {
		if _, ok := asking[comps.Items[i].Spec.ProfileRef.Name]; ok {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&comps.Items[i])})
		}
	}
	return out
}
