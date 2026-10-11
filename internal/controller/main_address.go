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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A website on the cluster's main address (routing.md §5).
//
// The main address is the cluster's bare domain, and on a single-tenancy
// cluster the one user tenant may put a public website there. Who holds it,
// and every condition that has to be true first, is internal/addresses: a
// pure function of the tenants, the profiles and the mode, which the three
// writers here all ask -- the website's own component (its route), the
// platform tenant's component on the bare domain (which steps back to the
// platform's paths) and the kernel's front door (which stops sending the
// front page to the desktop) -- and the director asks too, when it shows an
// approver the address an entry would be published at. This file is what the
// operator does with the answer.

// ConditionMainAddress is the Component condition that says whether one of
// its surfaces is on the cluster's main address, and why not when it is not.
const ConditionMainAddress = "MainAddress"

// The rule's names, as this package has always called them.
const (
	mainAddressBrandingPrefix = addresses.BrandingPrefix
	mainAddressSignInPrefix   = addresses.SignInPrefix
)

var mainAddressReservedPrefixes = addresses.ReservedPrefixes

type (
	mainAddressClaim   = addresses.Claim
	mainAddressInputs  = addresses.Inputs
	mainAddressVerdict = addresses.Verdict
)

func mainAddressPlatformPrefixes() []string { return addresses.PlatformPrefixes() }

func pathWithin(path, prefix string) bool { return addresses.PathWithin(path, prefix) }

func mainAddressHolder(in mainAddressInputs) *mainAddressClaim { return addresses.Holder(in) }

func mainAddressVerdictFor(in mainAddressInputs, tenant *gentianov1alpha1.Tenant, install string) (mainAddressVerdict, bool) {
	return addresses.VerdictFor(in, tenant, install)
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
