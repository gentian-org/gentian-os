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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// preApprovedEgress are the egress requests of profile that policy answers
// for an install of tenant: all of them where its catalog is listed in the
// PlatformSecurityPolicy's preApprovedEgressCatalogs, and each request by
// host whose hosts are all in a trusted domain, the cluster's
// (spec.trustedDomains) or the tenant's own while it was delegated one. Read
// on every reconcile, so the hold and the network policy agree with the
// policy as it is now.
func (r *ComponentReconciler) preApprovedEgress(ctx context.Context, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (map[string]struct{}, error) {
	return answeredEgress(ctx, r.Client, profile, tenant)
}

// answeredEgress is preApprovedEgress for any reader of the cluster.
func answeredEgress(ctx context.Context, c client.Client, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (map[string]struct{}, error) {
	if profile == nil || profile.Privileges() == nil || len(profile.Privileges().Egress) == 0 {
		return nil, nil
	}
	catalogs, err := security.LoadPreApprovedEgressCatalogs(ctx, c)
	if err != nil {
		return nil, err
	}
	trusted, err := security.LoadTrustedDomains(ctx, c)
	if err != nil {
		return nil, err
	}
	return mergeAnswered(
		security.PreApprovedEgress(profile, catalogs),
		security.TrustedEgress(profile, append(append([]string(nil), trusted...), security.TenantTrustedDomains(tenant)...)),
	), nil
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
// asks for egress when the PlatformSecurityPolicy changes: a catalog added to
// preApprovedEgressCatalogs lets a waiting install go on, and one removed
// takes its apps' egress away now, not when something else next wakes them.
func componentsAskingForEgressPrivileges(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
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
		if err := c.List(ctx, comps); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range comps.Items {
			if _, ok := asking[comps.Items[i].Spec.ProfileRef.Name]; ok {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&comps.Items[i])})
			}
		}
		return out
	})
}
