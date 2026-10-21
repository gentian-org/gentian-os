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
	"reflect"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The kernel realm as a hub for the tenants' administrators (iam.md 1.1c).
//
// Two things are declared in git and projected onto composites here:
//
//   - the Cluster claim names the security officers
//     (spec.platformAccess.securityOfficers): administrators of the cluster's
//     tenants, each an account of their tenant's realm. The tenant that
//     adopts the kernel realm gets them as its `hub`: per source tenant an
//     identity provider, per holder a linked kernel-realm account with its
//     one membership. Each source tenant gets `hubSpoke`, which composes the
//     client its administrators pass to the kernel realm and the gate on it.
//   - a Tenant may let the platform's administrators sign in to it
//     (spec.platformAccess.adminsEnter). It gets `kernelBroker`, which
//     composes the identity provider "kernel" in its realm; the hub gets the
//     tenant in `enter`, which composes the client that provider signs in
//     with.
//
// Read from the claim and not from a ConfigMap derived from it, for the
// reason the authorization projection gives: who holds a cluster role is
// written down in git, and a cluster must not be able to widen it from
// inside.

const (
	// hubSecondFactorRequired and hubSecondFactorOff are the two values of
	// spec.platformAccess.brokeredSecondFactor; the first is the default.
	hubSecondFactorRequired = "required"
	hubSecondFactorOff      = "off"
	// hubDefaultSecurityGroup is the claim's default for
	// platformRoles.securityOfficer, read from the claim when it is set.
	hubDefaultSecurityGroup = "gentian:platform:security"
)

// hubSubject is what a holder's subject may look like: Keycloak's user ids
// are UUIDs, and nothing longer or stranger is composed into a resource name.
// hubTenantName is a tenant's name, a DNS label.
var (
	hubSubject    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	hubTenantName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// platformAccess is spec.platformAccess of the Cluster claim, with the
// group the security officers hold.
type platformAccess struct {
	SecondFactor  string
	SecurityGroup string
	Officers      []securityOfficer
}

// securityOfficer is one entry of spec.platformAccess.securityOfficers.
type securityOfficer struct {
	Tenant, Subject, Username, Email string
}

// readPlatformAccess reads the claim. A cluster without the claim's kind, or
// without a claim, has nobody brokered: the zero value, with the defaults.
func (r *TenantReconciler) readPlatformAccess(ctx context.Context) (platformAccess, error) {
	out := platformAccess{SecondFactor: hubSecondFactorRequired, SecurityGroup: hubDefaultSecurityGroup}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: clusterClaimGVK.Group, Version: clusterClaimGVK.Version, Kind: clusterClaimGVK.Kind + "List",
	})
	if err := r.List(ctx, list, client.InNamespace(clusterConfigNamespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return out, nil
		}
		return out, err
	}
	for i := range list.Items {
		obj := list.Items[i].Object
		if group, _, _ := unstructured.NestedString(obj, "spec", "platformRoles", "securityOfficer"); group != "" {
			out.SecurityGroup = group
		}
		if v, _, _ := unstructured.NestedString(obj, "spec", "platformAccess", "brokeredSecondFactor"); v == hubSecondFactorOff {
			out.SecondFactor = hubSecondFactorOff
		}
		entries, _, _ := unstructured.NestedSlice(obj, "spec", "platformAccess", "securityOfficers")
		for _, e := range entries {
			m, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			o := securityOfficer{
				Tenant:   strings.TrimSpace(asString(m["tenant"])),
				Subject:  strings.TrimSpace(asString(m["subject"])),
				Username: strings.TrimSpace(asString(m["username"])),
				Email:    strings.TrimSpace(asString(m["email"])),
			}
			// An entry that cannot be composed is left out rather than
			// composed half: the claim's schema refuses these shapes, and a
			// claim written past the schema is not widened by guessing.
			if !hubTenantName.MatchString(o.Tenant) || !hubSubject.MatchString(o.Subject) ||
				o.Username == "" || len(o.Username) > 255 || len(o.Email) > 255 ||
				strings.ContainsAny(o.Username+o.Email, "\n\r\"'\\") {
				continue
			}
			out.Officers = append(out.Officers, o)
		}
	}
	return out, nil
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// hubProjection is what the composite of tenant carries of the hub: the
// hub itself for the tenant that adopts the kernel realm, and for any other
// tenant whether its administrators are brokered (hubSpoke) and whether the
// platform's administrators may enter it (kernelBroker).
//
// Nothing is projected on a cluster that names no kernel realm or domain:
// there is nothing to broker to.
func (r *TenantReconciler) hubProjection(ctx context.Context, tenant *gentianov1alpha1.Tenant) (hub map[string]interface{}, spoke, kernelBroker bool, err error) {
	if r.KernelRealm == "" || r.KernelDomain == "" || tenant.DeletionTimestamp != nil {
		return nil, false, false, nil
	}
	access, err := r.readPlatformAccess(ctx)
	if err != nil {
		return nil, false, false, err
	}
	if !r.adoptsKernelRealm(tenant) {
		for _, o := range access.Officers {
			if o.Tenant == tenant.Name {
				spoke = true
				break
			}
		}
		return nil, spoke, tenant.PlatformAdminsEnter(), nil
	}

	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, false, false, err
	}
	byName := map[string]*gentianov1alpha1.Tenant{}
	for i := range list.Items {
		t := &list.Items[i]
		// A tenant on its way out, or one that is itself the kernel realm's,
		// is no source: nothing of it is brokered.
		if t.DeletionTimestamp == nil && !r.adoptsKernelRealm(t) {
			byName[t.Name] = t
		}
	}

	spokes := []interface{}{}
	holdersByTenant := map[string][]interface{}{}
	seen := map[string]bool{}
	for _, o := range access.Officers {
		if _, ok := byName[o.Tenant]; !ok || seen[o.Tenant+"/"+o.Subject] {
			continue
		}
		seen[o.Tenant+"/"+o.Subject] = true
		holdersByTenant[o.Tenant] = append(holdersByTenant[o.Tenant], map[string]interface{}{
			"subject": o.Subject, "username": o.Username, "email": o.Email,
		})
	}
	names := make([]string, 0, len(holdersByTenant))
	for n := range holdersByTenant {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := byName[n]
		holders := holdersByTenant[n]
		sort.Slice(holders, func(i, j int) bool {
			return holders[i].(map[string]interface{})["subject"].(string) < holders[j].(map[string]interface{})["subject"].(string)
		})
		display := t.Spec.DisplayName
		if display == "" {
			display = t.Name
		}
		spokes = append(spokes, map[string]interface{}{
			"tenant": t.Name, "realm": tenantRealmName(t), "displayName": display, "holders": holders,
		})
	}

	enter := []interface{}{}
	entering := make([]string, 0)
	for n, t := range byName {
		if t.PlatformAdminsEnter() {
			entering = append(entering, n)
		}
	}
	sort.Strings(entering)
	for _, n := range entering {
		enter = append(enter, map[string]interface{}{"tenant": n, "realm": tenantRealmName(byName[n])})
	}

	hub = map[string]interface{}{
		"secondFactor":  access.SecondFactor,
		"securityGroup": access.SecurityGroup,
		"spokes":        spokes,
		"enter":         enter,
	}
	return hub, false, false, nil
}

// platformAccessChanged lets a claim event through only when what the hub
// composes from it changed: the holders, the second factor, or the group.
func platformAccessChanged() predicate.Predicate {
	of := func(obj client.Object) []interface{} {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return nil
		}
		access, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "platformAccess")
		group, _, _ := unstructured.NestedString(u.Object, "spec", "platformRoles", "securityOfficer")
		return []interface{}{access, group}
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return !reflect.DeepEqual(of(e.ObjectOld), of(e.ObjectNew)) },
	}
}

// tenantHubFactsChanged lets a Tenant event through when what the hub
// composes of that tenant changed: whether the platform's administrators may
// enter it, what it is called, which realm it is in, and whether it exists.
func tenantHubFactsChanged() predicate.Predicate {
	of := func(obj client.Object) []interface{} {
		t, ok := obj.(*gentianov1alpha1.Tenant)
		if !ok {
			return nil
		}
		return []interface{}{t.PlatformAdminsEnter(), t.Spec.DisplayName, tenantRealmName(t), t.DeletionTimestamp != nil}
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return !reflect.DeepEqual(of(e.ObjectOld), of(e.ObjectNew)) },
	}
}

// tenantsAdoptingKernelRealm maps any event to the tenants whose realm is
// the kernel realm: the hub is theirs to recompose.
func tenantsAdoptingKernelRealm(c client.Client, kernelRealm string) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		if kernelRealm == "" {
			return nil
		}
		list := &gentianov1alpha1.TenantList{}
		if err := c.List(ctx, list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			if tenantRealmName(&list.Items[i]) == kernelRealm {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: list.Items[i].Name}})
			}
		}
		return out
	})
}
