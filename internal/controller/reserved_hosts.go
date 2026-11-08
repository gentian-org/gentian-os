/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/hostnames"
)

// reservedHostRefusal is why a component may not be served in this zone, or
// "" when it may: one of its entries would answer on an address name the
// platform depends on.
//
// The names, who may hold one, and how an entry is matched are
// internal/hostnames', which the director asks the same question of before
// it commits an install. Two tiers: the names kept in every tenant (the
// desktop's, the administration console's, the App Store's, and the ones a
// sign-in page would have), and the kernel's own hosts, which are asked only
// of a tenant whose domain IS the cluster's -- the user tenant of a
// single-tenancy cluster.
//
// The component is held whole. An app whose address was refused has no use,
// and one served on some of its entries and not on others would be an app
// that half works with the reason on a condition nobody opened.
func reservedHostRefusal(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, zone zoneNames, kernelDomain string) string {
	refusal := hostnames.Check(comp.Name, profile, reservedHostZone(zone, kernelDomain))
	if refusal == nil {
		return ""
	}
	return refusal.Message() + ". Nothing is installed or routed"
}

// reservedHostZone is what the list has to know of a zone: its domain, and
// whether that is the cluster's own. The platform tenant's is not -- its
// hosts are below platform.<kernel>.
func reservedHostZone(zone zoneNames, kernelDomain string) hostnames.Zone {
	return addresses.ReservedZone(zone.shared(), kernelDomain)
}
