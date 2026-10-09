/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package addresses is where a component's entries answer: the host of each,
// the paths a perimeter entry publishes, and who holds the cluster's main
// address.
//
// Two programs have to give the same answer. The operator writes the route
// and the listener for a host; the director shows a perimeter approver the
// address an entry would be published at before they approve it. An address
// shown that is not the address published would have somebody approve one
// thing and get another, so the rule is here once and both import it; neither
// has a copy.
//
// What the two know differs, and the rule takes it as arguments. The operator
// passes the Tenant as the cluster holds it. The director passes one built
// from git: the same name and realm, with status.domain set to the custom
// domain the repository binds.
package addresses

import (
	"sort"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/hostnames"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// DesktopLabel is the host label of a tenant's desktop.
const DesktopLabel = "desktop"

// Zone is where one tenant's hosts are.
type Zone struct {
	// Domain is what the zone's hosts are under: <subDomain>.<Domain>.
	//   platform tenant            platform.<kernel>
	//   a tenant, tenancy multi    <tenant>.<kernel>, or its custom domain
	//   the user tenant, single    <kernel> itself
	Domain string
	// Kernel marks the platform tenant's zone, whose session is the kernel
	// realm's.
	Kernel bool
	// Apex is where an apex entry answers: the cluster's bare domain, for the
	// platform tenant, and nowhere for anybody else -- the user tenant's
	// website on a single-tenancy cluster is given that address one entry at
	// a time, by Holder, and not through here.
	Apex string
}

// AdoptsKernelRealm reports the platform tenant: the one whose people are in
// the kernel realm.
func AdoptsKernelRealm(tenant *gentianov1alpha1.Tenant, kernelRealm string) bool {
	return kernelRealm != "" && keycloak.RealmName(tenant) == kernelRealm
}

// ZoneOf is where one tenant's hosts are, under this cluster's mode.
func ZoneOf(tenant *gentianov1alpha1.Tenant, kernelDomain, tenancyMode, kernelRealm string) Zone {
	zone := Zone{Domain: tenant.EffectiveDomain(kernelDomain, tenancyMode)}
	if AdoptsKernelRealm(tenant, kernelRealm) {
		zone.Kernel = true
		zone.Apex = kernelDomain
	}
	return zone
}

// Host is where an entry answers: a label under the zone's domain, the
// entry's own or the component's name.
//
// Two entries are not a label under the domain.
//
// The desktop's, in the platform tenant's zone, answers on the zone's domain
// itself: platform.<kernel> is the platform administrator's desktop, and
// desktop.<kernel> is not the platform's at all -- it is the user tenant's
// desktop on a single-tenancy cluster and an alias of the bare domain on a
// multi-tenancy one.
//
// An apex entry answers on the cluster's bare domain, and only where the zone
// has it (Zone.Apex). It has no host anywhere else, which is the empty string
// here and "not published" to every caller: a tenant's bare domain is its own
// to route.
func Host(zone Zone, component string, e *gentianov1alpha1.ExposureSpec) string {
	if zone.Domain == "" {
		return ""
	}
	if e.Apex {
		return zone.Apex
	}
	sub := e.SubDomain
	if sub == "" {
		sub = component
	}
	if zone.Kernel && sub == DesktopLabel {
		return zone.Domain
	}
	return sub + "." + zone.Domain
}

// PerimeterHost is where one perimeter entry of a component is published once
// it is approved, and whether it is there as a tenant's website on the
// cluster's main address.
//
// approvedForMain is the approver's own word (apex on the enablement).
// mainEntry is the entry of this component that holds the main address, as
// VerdictFor decided, and mainHost that address; both empty when none does.
//
// The main address takes two people saying so: the profile's author (apex on
// the entry) and the approver. One without the other publishes nothing, here
// or under the tenant's own hosts. The platform tenant's zone is not asked:
// its page is on the bare domain either way. An empty host is an entry with
// nowhere to answer -- the bare domain, asked for by a tenant that does not
// hold it.
func PerimeterHost(
	zone Zone, component string, e *gentianov1alpha1.ExposureSpec,
	approvedForMain bool, mainEntry, mainHost string,
) (host string, website bool) {
	host = Host(zone, component, e)
	if !zone.Kernel {
		if e.Apex != approvedForMain {
			return "", false
		}
		if e.Apex && mainEntry == e.Name {
			host, website = mainHost, true
		}
	}
	return host, website
}

// Prefixes are the path prefixes a perimeter entry publishes, sorted so the
// generated configuration does not churn between reconciles.
//
// An entry that declares none publishes NOTHING. That is deliberate and it is
// the opposite of the gateway surface, where empty paths mean the whole host:
// on the perimeter, "I did not say" must never resolve to "everything".
func Prefixes(e *gentianov1alpha1.ExposureSpec) []string {
	out := make([]string, 0, len(e.Paths))
	for _, p := range e.Paths {
		if p = strings.TrimSpace(p); p != "" && strings.HasPrefix(p, "/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// onClusterDomain reports a tenant whose domain is the cluster's own.
func onClusterDomain(effectiveDomain, kernelDomain string) bool {
	return kernelDomain != "" && effectiveDomain == kernelDomain
}

// ReservedZone is what the list of names an app may not take
// (internal/hostnames) has to know of a zone: its domain, and whether that is
// the cluster's own. The platform tenant's is not -- its hosts are below
// platform.<kernel>.
func ReservedZone(zone Zone, kernelDomain string) hostnames.Zone {
	return hostnames.Zone{
		Domain:          zone.Domain,
		OnClusterDomain: !zone.Kernel && onClusterDomain(zone.Domain, kernelDomain),
	}
}

// Resolve is where one perimeter entry of one of a tenant's components is
// published, given what the tenant has approved (tenant.Spec.Exposures) and
// what the approver says of this entry (approvedForMain: apex on the
// enablement). An empty host is an entry that is published nowhere, and why
// says so in a sentence.
//
// It is the operator's decision in the operator's order, from the same parts:
// a component held for an address name the platform keeps publishes nothing
// (hostnames.Check); the main address is VerdictFor's; the host is
// PerimeterHost's; and an entry with no paths publishes nothing (Prefixes).
// The operator comes to it in two places -- the listener and the route -- and
// a test holds this to both.
//
// Whether an approval has expired is not asked here, except for the main
// address, where an expired approval no longer holds it.
func Resolve(
	in Inputs, tenant *gentianov1alpha1.Tenant, install string,
	entry *gentianov1alpha1.ExposureSpec, approvedForMain bool,
) (host, why string) {
	zone := ZoneOf(tenant, in.KernelDomain, in.TenancyMode, in.KernelRealm)
	if refusal := hostnames.Check(install, in.Profiles[install], ReservedZone(zone, in.KernelDomain)); refusal != nil {
		return "", refusal.Message() + ". Nothing is published"
	}
	if len(Prefixes(entry)) == 0 {
		return "", install + "/" + entry.Name + " declares no paths, so it publishes nothing"
	}
	mainEntry, mainHost, mainWhy := "", "", ""
	if !zone.Kernel {
		if verdict, applies := VerdictFor(in, tenant, install); applies {
			if verdict.Entry != "" {
				mainEntry, mainHost = verdict.Entry, in.KernelDomain
			}
			mainWhy = verdict.Message
		}
	}
	host, _ = PerimeterHost(zone, install, entry, approvedForMain, mainEntry, mainHost)
	if host != "" {
		return host, ""
	}
	switch {
	case zone.Domain == "":
		return "", "this cluster declares no domain, so nothing has an address"
	case entry.Apex && !approvedForMain:
		return "", install + "/" + entry.Name + " is declared for the cluster's main address and is published only when the approver says so. Nothing is published"
	case !entry.Apex && approvedForMain:
		return "", install + "/" + entry.Name + " was approved for the cluster's main address, but the profile does not declare it for that address. Nothing is published"
	case mainWhy != "":
		return "", mainWhy
	}
	return "", install + "/" + entry.Name + " is for the cluster's main address, which this tenant does not hold. Nothing is published"
}
