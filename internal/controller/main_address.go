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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A website on the cluster's main address (routing.md §5).
//
// The main address is the cluster's bare domain. On a single-tenancy cluster
// the one user tenant may put a public website there. It is the most visible
// page of the cluster and nobody is signed in on it, so every condition is
// explicit and all of them must hold:
//
//   - the cluster's tenancy mode is single, and the tenant is the user tenant
//     on the cluster's own domain;
//   - the profile's entry is a perimeter one that says apex;
//   - the tenant's perimeter approver published it and said apex too;
//   - it has not expired, and it stays off the paths the platform keeps;
//   - the tenant is Ready, and no earlier surface already holds the address.
//
// Three writers have to agree on who holds it: the website's own component
// (its route), the platform tenant's component on the bare domain (which
// steps back to the platform's paths) and the kernel's front door (which
// stops sending the front page to the desktop). They all ask
// mainAddressHolder, a pure function of the tenants, the profiles and the
// mode, so they come to the same answer.

// ConditionMainAddress is the Component condition that says whether one of
// its surfaces is on the cluster's main address, and why not when it is not.
const ConditionMainAddress = "MainAddress"

// The platform's paths on the main address. A website never serves them.
//
//	/branding/                     the brand files every console loads
//	/sign-in                       always leads to sign-in
//	/.well-known/acme-challenge/   answering it proves control of the domain
//	/.well-known/pki-validation/   to a certificate authority; so does this
//
// Everything else under /.well-known/ is the website's.
const (
	mainAddressBrandingPrefix = "/branding/"
	mainAddressSignInPrefix   = "/sign-in"
)

var mainAddressReservedPrefixes = []string{
	mainAddressBrandingPrefix,
	mainAddressSignInPrefix,
	"/.well-known/acme-challenge/",
	"/.well-known/pki-validation/",
}

// mainAddressPlatformPrefixes are the reserved paths the platform's own page
// is still routed for while a website holds the address. /sign-in is not
// among them: the front door redirects it to the desktop.
func mainAddressPlatformPrefixes() []string {
	var out []string
	for _, p := range mainAddressReservedPrefixes {
		if p != mainAddressSignInPrefix {
			out = append(out, p)
		}
	}
	return out
}

// pathWithin reports a path prefix that lies inside another: equal to it, or
// below it by whole segments.
func pathWithin(path, prefix string) bool {
	path, prefix = strings.TrimSuffix(path, "/"), strings.TrimSuffix(prefix, "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// reservedMainAddressPath is the platform's path a declared prefix would be
// inside, or "". A prefix above a reserved one (/ itself, /.well-known/) is
// fine: the reserved path is longer and so outranks it at the Gateway.
func reservedMainAddressPath(e *gentianov1alpha1.ExposureSpec) string {
	for _, p := range perimeterPrefixes(e) {
		for _, reserved := range mainAddressReservedPrefixes {
			if pathWithin(p, reserved) {
				return reserved
			}
		}
	}
	return ""
}

// mainAddressClaim is one surface a tenant's approver published for the main
// address.
type mainAddressClaim struct {
	Tenant, Install, Exposure string
}

func (c mainAddressClaim) String() string { return c.Install + "/" + c.Exposure }

// mainAddressInputs is what decides who holds the main address.
type mainAddressInputs struct {
	Tenants      []gentianov1alpha1.Tenant
	Profiles     map[string]*gentianov1alpha1.ComponentProfile
	KernelDomain string
	KernelRealm  string
	TenancyMode  string
	Now          time.Time
}

// userTenant is the one user tenant of a single-tenancy cluster, or nil.
func (in mainAddressInputs) userTenant() *gentianov1alpha1.Tenant {
	if gentianov1alpha1.NormalizeTenancyMode(in.TenancyMode) != gentianov1alpha1.TenancyModeSingle {
		return nil
	}
	for i := range in.Tenants {
		t := &in.Tenants[i]
		if t.DeletionTimestamp == nil && t.Name == gentianov1alpha1.SingleUserTenantName &&
			!tenantAdoptsKernelRealm(t, in.KernelRealm) {
			return t
		}
	}
	return nil
}

// apexEntry is the profile's perimeter entry of this name that says apex.
func apexEntry(profile *gentianov1alpha1.ComponentProfile, name string) *gentianov1alpha1.ExposureSpec {
	if profile == nil {
		return nil
	}
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Name == name && e.Surface == gentianov1alpha1.SurfacePerimeter && e.Apex {
			return e
		}
	}
	return nil
}

// mainAddressRefusal is why one published surface is not on the main
// address, as a reason and a sentence for the tenant's administrator; both
// empty when nothing but another claim could stand in its way.
func (in mainAddressInputs) refusal(tenant *gentianov1alpha1.Tenant, e *gentianov1alpha1.TenantExposure) (reason, message string) {
	entry := apexEntry(in.Profiles[e.Install], e.ExposureName)
	switch {
	case entry == nil:
		return "NotAMainAddressEntry", fmt.Sprintf(
			"%s/%s was published for the cluster's main address, but the profile does not declare it as a perimeter entry with apex: true. Nothing is published",
			e.Install, e.ExposureName)
	case !e.Apex:
		return "NotRequested", fmt.Sprintf(
			"%s/%s is declared for the cluster's main address and is published only when the approver says so (apex: true on the request). Nothing is published",
			e.Install, e.ExposureName)
	case gentianov1alpha1.NormalizeTenancyMode(in.TenancyMode) != gentianov1alpha1.TenancyModeSingle:
		return "MultiTenancy", "this cluster's tenancy mode is multi: its main address is the sign-in form and no tenant's website. Nothing is published"
	case in.userTenant() == nil || in.userTenant().Name != tenant.Name:
		return "NotTheUserTenant", fmt.Sprintf(
			"only the user tenant %q may put a website on the cluster's main address. Nothing is published",
			gentianov1alpha1.SingleUserTenantName)
	case !servedByKernelEdge(tenant.EffectiveDomain(in.KernelDomain, in.TenancyMode), in.KernelDomain):
		return "OwnDomain", "this tenant is on a domain of its own, so the cluster's main address is not its address. Nothing is published"
	case e.ExpiresAt != nil && !in.Now.Before(e.ExpiresAt.Time):
		return "Expired", fmt.Sprintf("the approval of %s/%s for the main address has expired. Nothing is published", e.Install, e.ExposureName)
	case len(perimeterPrefixes(entry)) == 0:
		return "NoPaths", fmt.Sprintf("%s/%s declares no paths, so it publishes nothing", e.Install, e.ExposureName)
	}
	if reserved := reservedMainAddressPath(entry); reserved != "" {
		return "ReservedPath", fmt.Sprintf(
			"%s/%s declares a path inside %s, which the platform keeps on the main address (%s). Nothing is published",
			e.Install, e.ExposureName, reserved, strings.Join(mainAddressReservedPrefixes, ", "))
	}
	if tenant.Status.Phase != gentianov1alpha1.TenantPhaseReady {
		return "TenantNotReady", "the tenant is not Ready yet; the main address changes once it is"
	}
	return "", ""
}

// mainAddressHolder is the surface that holds the cluster's main address, or
// nil when none does.
//
// One at a time. Where several were published for it, the one published
// first holds it and keeps it: a later request cannot take a running website
// off the address.
func mainAddressHolder(in mainAddressInputs) *mainAddressClaim {
	tenant := in.userTenant()
	if tenant == nil {
		return nil
	}
	type candidate struct {
		claim mainAddressClaim
		at    *metav1.Time
	}
	var live []candidate
	for i := range tenant.Spec.Exposures {
		e := &tenant.Spec.Exposures[i]
		if !e.Apex {
			continue
		}
		if reason, _ := in.refusal(tenant, e); reason != "" {
			continue
		}
		live = append(live, candidate{
			claim: mainAddressClaim{Tenant: tenant.Name, Install: e.Install, Exposure: e.ExposureName},
			at:    e.PublishedAt,
		})
	}
	if len(live) == 0 {
		return nil
	}
	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i].at, live[j].at
		switch {
		case a != nil && b != nil && !a.Equal(b):
			return a.Before(b)
		case (a == nil) != (b == nil):
			return a != nil
		}
		return live[i].claim.String() < live[j].claim.String()
	})
	return &live[0].claim
}

// mainAddressVerdict is what the tenant's administrator reads on a component
// one of whose surfaces is for the main address.
type mainAddressVerdict struct {
	// Entry is the profile entry that holds the main address; empty when
	// none of this component's does.
	Entry           string
	Reason, Message string
}

// mainAddressVerdictFor decides for one component of one tenant. applies is
// false for a component that has nothing to do with the main address.
func mainAddressVerdictFor(in mainAddressInputs, tenant *gentianov1alpha1.Tenant, install string) (verdict mainAddressVerdict, applies bool) {
	holder := mainAddressHolder(in)
	for i := range tenant.Spec.Exposures {
		e := &tenant.Spec.Exposures[i]
		if e.Install != install {
			continue
		}
		if !e.Apex && apexEntry(in.Profiles[install], e.ExposureName) == nil {
			continue
		}
		if reason, message := in.refusal(tenant, e); reason != "" {
			verdict, applies = mainAddressVerdict{Reason: reason, Message: message}, true
			continue
		}
		if holder != nil && holder.Tenant == tenant.Name && holder.Install == install && holder.Exposure == e.ExposureName {
			return mainAddressVerdict{
				Entry:  e.ExposureName,
				Reason: "Published",
				Message: fmt.Sprintf(
					"%s/%s is published on the cluster's main address, https://%s/. The front page shows it once its proxy is up; %s on that address stay the platform's, and https://%s%s always leads to sign-in",
					install, e.ExposureName, in.KernelDomain, strings.Join(mainAddressReservedPrefixes, ", "),
					in.KernelDomain, mainAddressSignInPrefix),
			}, true
		}
		verdict, applies = mainAddressVerdict{
			Reason: "HeldByAnother",
			Message: fmt.Sprintf(
				"the cluster's main address is already held by %s, and one surface holds it at a time. Withdraw that one first. Nothing is published",
				holder),
		}, true
	}
	return verdict, applies
}

// mainAddressProxyName is the publishing proxy of the surface that holds the
// main address, and the namespace it runs in.
func mainAddressProxyName(c *mainAddressClaim) (namespace, name string) {
	comp := &gentianov1alpha1.Component{}
	comp.Name = c.Install
	return layout.TenantDMZ(c.Tenant), perimeterName(comp, c.Exposure)
}

// mainAddressChanged passes the Tenant events that can change who holds the
// main address: what a tenant publishes, and whether it is Ready.
func mainAddressChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*gentianov1alpha1.Tenant)
			after, ok2 := e.ObjectNew.(*gentianov1alpha1.Tenant)
			if !ok1 || !ok2 {
				return false
			}
			return before.Status.Phase != after.Status.Phase ||
				before.Status.Domain != after.Status.Domain ||
				!equality.Semantic.DeepEqual(before.Spec.Exposures, after.Spec.Exposures)
		},
	}
}

// componentsOnTheMainAddress re-runs the components the main address is
// about: those whose profile has an apex entry, in any tenant.
func componentsOnTheMainAddress(c client.Reader) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		profiles := &gentianov1alpha1.ComponentProfileList{}
		if err := c.List(ctx, profiles); err != nil {
			return nil
		}
		apex := map[string]struct{}{}
		for i := range profiles.Items {
			for j := range profiles.Items[i].Spec.Expose {
				e := &profiles.Items[i].Spec.Expose[j]
				if e.Surface == gentianov1alpha1.SurfacePerimeter && e.Apex {
					apex[profiles.Items[i].Name] = struct{}{}
				}
			}
		}
		if len(apex) == 0 {
			return nil
		}
		components := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, components); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range components.Items {
			comp := &components.Items[i]
			if _, ok := apex[comp.Spec.ProfileRef.Name]; ok {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}})
			}
		}
		return out
	})
}
