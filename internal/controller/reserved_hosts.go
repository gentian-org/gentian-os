/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// kernelReservedHostLabels are the names directly under the cluster's domain
// that the kernel and the platform tenant answer on, on every cluster:
//
//	argocd, headlamp, llm   the kernel's consoles (kernel_gateway_routes.go)
//	id                      the identity provider
//	platform                the platform administrator's desktop, and
//	                        everything of the platform tenant below it
//	www                     an alias of the bare domain
//	mail, imap, mail-egress the cluster's mail hosts (mail_dnsendpoint.go)
//	corp                    a name the tunnel ingress publishes for the cluster
//
// On a multi-tenancy cluster no tenant can reach them: a tenant's hosts are
// <label>.<tenant>.<kernel> or on its custom domain. On a single-tenancy
// cluster the user tenant's hosts are <label>.<kernel>, the same level, and a
// component or an app whose label is one of these would claim the kernel's
// own address -- the more specific route wins by age, not by who should have
// it, and "the identity provider's host now serves somebody's app" has no
// error anywhere. So it is refused before anything is written.
//
// desktop and admin are deliberately not here: on a single-tenancy cluster
// they are the user tenant's desktop and administration console.
var kernelReservedHostLabels = []string{
	"argocd", "corp", "headlamp", "id", "imap", "llm", "mail", "mail-egress",
	gentianov1alpha1.PlatformTenantName, "www",
}

// reservedHostLabel is the kernel's name a host label would take, or "".
// A label may be several: a.platform is under platform, and is the kernel's
// as much as platform itself.
func reservedHostLabel(label string) string {
	for _, reserved := range kernelReservedHostLabels {
		if label == reserved || strings.HasSuffix(label, "."+reserved) {
			return reserved
		}
	}
	return ""
}

// reservedHostRefusal is why a component may not be served in this zone, or
// "" when it may: one of its entries would answer on a name directly under
// the cluster's domain that is the kernel's.
//
// Asked only of a tenant whose domain IS the cluster's -- the user tenant of
// a single-tenancy cluster. Every entry that would be routed or published is
// looked at, by the label its host is built from (exposureHostIn): its
// subDomain, or the component's name. An entry served by another component
// (an addon's) has that component's host and is that component's to answer
// for.
func reservedHostRefusal(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, zone zoneNames, kernelDomain string) string {
	if zone.kernel || !servedByKernelEdge(zone.domain, kernelDomain) {
		return ""
	}
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Apex || (e.Backend.Component != "" && e.Backend.Component != comp.Name) {
			continue
		}
		label := e.SubDomain
		if label == "" {
			label = comp.Name
		}
		if reserved := reservedHostLabel(label); reserved != "" {
			return fmt.Sprintf(
				"exposure %q would answer on %s.%s, and %s.%s is the platform's own address on this cluster: "+
					"its tenancy mode is single, so this tenant's hosts are directly under the cluster's domain. "+
					"Nothing is installed or routed. The names a component may not take here are: %s",
				e.Name, label, kernelDomain, reserved, kernelDomain, strings.Join(kernelReservedHostLabels, ", "))
		}
	}
	return ""
}
