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
	"slices"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// One tenant's people signing in to another of the same cluster.
//
// It takes both tenants: the target names the source in spec.signInFrom, with
// the groups of the target its people are in, and the source names the target
// in spec.signInTo. Only where both say so does anything reach either
// composite:
//
//   - the source's gets one more brokeredTo entry, the client the target's
//     realm signs in with (broker-to-<target>, authenticated by the JWT the
//     target realm signs, checked against its published keys);
//   - the target's gets a signInFrom entry: the identity provider
//     tenant-<source> and one mapper per group.
//
// Everything a person could otherwise have mistyped in two consoles follows
// from the two tenants' names and the kernel domain.

// signInMemberGroup is the one platform group a sign-in may give: the
// tenant's members, who may enter it (tenant#member). Its administrators and
// perimeter approvers are never given this way.
const signInMemberGroup = "members"

// signInAlias is the target realm's identity provider for a source tenant,
// and the path segment of its broker endpoint.
func signInAlias(source string) string { return "tenant-" + source }

// tenantRealmName is the realm a tenant's people are in.
func tenantRealmName(t *gentianov1alpha1.Tenant) string {
	if t.Spec.Isolation != nil && t.Spec.Isolation.KeycloakRealm != "" {
		return t.Spec.Isolation.KeycloakRealm
	}
	return t.Name
}

// signInGroupPaths are a sign-in's groups as the target realm's paths, in
// the order given: members as the tenant's member group, a declared group by
// its name. A name the target does not declare is left out.
func signInGroupPaths(target *gentianov1alpha1.Tenant, groups []string) []interface{} {
	declared := map[string]bool{}
	for _, g := range target.Spec.Groups {
		declared[g.Name] = true
	}
	var out []interface{}
	seen := map[string]bool{}
	for _, g := range groups {
		var path string
		switch {
		case g == signInMemberGroup:
			path = fmt.Sprintf("/gentian:tenant:%s:members", target.Name)
		case declared[g]:
			path = "/" + g
		default:
			continue
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

// tenantSignInProjection is what the composite of tenant carries for the
// sign-ins it takes part in: the identity providers it is the target of, and
// the clients it is the source of.
func (r *TenantReconciler) tenantSignInProjection(ctx context.Context, tenant *gentianov1alpha1.Tenant) (signInFrom, brokeredTo []interface{}, err error) {
	if len(tenant.Spec.SignInFrom) == 0 && len(tenant.Spec.SignInTo) == 0 {
		return nil, nil, nil
	}
	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, nil, err
	}
	byName := map[string]*gentianov1alpha1.Tenant{}
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp == nil {
			byName[list.Items[i].Name] = &list.Items[i]
		}
	}
	kd := r.KernelDomain
	// As the target: each source that consents.
	for _, from := range tenant.Spec.SignInFrom {
		source, ok := byName[from.Tenant]
		if !ok || from.Tenant == tenant.Name || !slices.Contains(source.Spec.SignInTo, tenant.Name) {
			continue
		}
		display := source.Spec.DisplayName
		if display == "" {
			display = source.Name
		}
		signInFrom = append(signInFrom, map[string]interface{}{
			"tenant":      source.Name,
			"alias":       signInAlias(source.Name),
			"displayName": display,
			"realm":       tenantRealmName(source),
			"clientID":    "broker-to-" + tenant.Name,
			"groups":      signInGroupPaths(tenant, from.Groups),
		})
	}
	// As the source: each target that names this tenant, and that this one
	// names back.
	targets := append([]string(nil), tenant.Spec.SignInTo...)
	sort.Strings(targets)
	for _, name := range slices.Compact(targets) {
		target, ok := byName[name]
		if !ok || name == tenant.Name || kd == "" {
			continue
		}
		named := slices.ContainsFunc(target.Spec.SignInFrom, func(f gentianov1alpha1.TenantSignInFrom) bool {
			return f.Tenant == tenant.Name
		})
		if !named {
			continue
		}
		realm := tenantRealmName(target)
		brokeredTo = append(brokeredTo, map[string]interface{}{
			"name":        target.Name,
			"redirectURI": fmt.Sprintf("https://id.%s/auth/realms/%s/broker/%s/endpoint", kd, realm, signInAlias(tenant.Name)),
			"jwksURL":     fmt.Sprintf("https://id.%s/auth/realms/%s/protocol/openid-connect/certs", kd, realm),
		})
	}
	return signInFrom, brokeredTo, nil
}

// tenantsSignInWith maps a tenant to the tenants a sign-in pairs it with, in
// either direction: a change of one side is the other's composite to redo.
func tenantsSignInWith(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		changed, ok := obj.(*gentianov1alpha1.Tenant)
		if !ok {
			return nil
		}
		names := map[string]bool{}
		for _, n := range changed.Spec.SignInTo {
			names[n] = true
		}
		for _, f := range changed.Spec.SignInFrom {
			names[f.Tenant] = true
		}
		list := &gentianov1alpha1.TenantList{}
		if err := c.List(ctx, list); err == nil {
			for i := range list.Items {
				t := &list.Items[i]
				if slices.Contains(t.Spec.SignInTo, changed.Name) ||
					slices.ContainsFunc(t.Spec.SignInFrom, func(f gentianov1alpha1.TenantSignInFrom) bool { return f.Tenant == changed.Name }) {
					names[t.Name] = true
				}
			}
		}
		delete(names, changed.Name)
		out := make([]reconcile.Request, 0, len(names))
		for n := range names {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: n}})
		}
		return out
	})
}
