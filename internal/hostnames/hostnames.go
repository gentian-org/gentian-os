/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package hostnames is the list of address names an app may not take, and
// the one check every door asks it through.
//
// A component is published at <label>.<its tenant's domain>, and the label
// is the profile's to state (exposure subDomain, or the component's name).
// A profile from a catalogue is not written by the platform, so the name is
// not trusted from it: two routes that claim one host are resolved by the
// Gateway by age, silently, and an app at desktop.<tenant> would be the page
// a tenant's people believe is their desktop. The names the platform depends
// on are therefore refused to everything but the platform's own component
// for each -- by the operator before anything is written for a component,
// and by the director before an install is committed. Both import this
// package; neither has a list of its own.
package hostnames

import (
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// Reserved is one name an app may not take.
type Reserved struct {
	// Label is the host label: <Label>.<domain>, and everything under it.
	Label string
	// Owner is the ComponentProfile the platform ships that answers there,
	// or "" for a name nothing may take.
	Owner string
	// Why is the one-line reason, as the documents state it.
	Why string
}

// platformLabels are reserved in every tenant, on every cluster, under the
// tenant's domain whatever that is: <tenant>.<cluster>, the cluster's own
// domain for the user tenant of a single-tenancy cluster, or a custom domain.
//
// Three are held by a component the platform ships and may be taken by that
// component alone. The others are kept free: nothing answers on them, so
// that no app can stand where people expect the platform -- its former
// address, or a name a sign-in or account page would have.
//
// Short on purpose. Names that are an app's business (mail, chat, files,
// docs, wiki, api, www -- a tenant's website is rightly www.<its domain>)
// are not here, and neither is operations: the Operations Console is a
// catalogue component like any other, and reserving its label would refuse
// it. Matching is exact; there is no look-alike matching (desktop1,
// desk-top), because no rule for it is well defined.
var platformLabels = []Reserved{
	{Label: "desktop", Owner: "desktop", Why: "the tenant's desktop"},
	{Label: "admin", Owner: "admin-console", Why: "the tenant's administration console"},
	{Label: "store", Owner: "app-store", Why: "the App Store app"},
	{Label: "console", Why: "the desktop's former address, which people may still type"},
	{Label: gentianov1alpha1.PlatformTenantName, Why: "reads as the platform administrator's desktop, platform.<cluster>"},
	{Label: "id", Why: "reads as the identity provider, id.<cluster>"},
	{Label: "auth", Why: "a name a sign-in page would have"},
	{Label: "login", Why: "a name a sign-in page would have"},
	{Label: "signin", Why: "a name a sign-in page would have"},
	{Label: "sign-in", Why: "a name a sign-in page would have"},
	{Label: "sso", Why: "a name a sign-in page would have"},
	{Label: "account", Why: "a name a page for one's account and password would have"},
	{Label: "accounts", Why: "a name a page for one's account and password would have"},
}

// kernelLabels are the names directly under the cluster's domain that the
// kernel and the platform tenant answer on, on every cluster. They are
// refused where a tenant's domain IS the cluster's -- the user tenant of a
// single-tenancy cluster -- because there a component's host is at the same
// level and would claim the kernel's own address.
//
// Not refused in a tenant with a domain of its own: llm.acme.<cluster> is
// not the kernel's host and takes nothing from it, and mail or imap there is
// an app's business. The ones that would help deceive people under any
// domain (id, platform) are in platformLabels as well, and so are refused
// everywhere.
//
// The list is what writes a host under the cluster's domain:
// kernel_gateway_routes.go (id, www, argocd, headlamp, llm),
// mail_dnsendpoint.go and the mail egress record (mail, imap, mail-egress),
// gateway_tunnel_ingress.go (corp), and the platform tenant's zone
// (platform, and admin.platform below it). The object store, the secret
// store and the registry publish no host.
var kernelLabels = []Reserved{
	{Label: "argocd", Why: "the GitOps console"},
	{Label: "corp", Why: "a name the tunnel ingress publishes for the cluster"},
	{Label: "headlamp", Why: "the cluster console"},
	{Label: "id", Why: "the identity provider"},
	{Label: "imap", Why: "the cluster's mail host for reading mail"},
	{Label: "llm", Why: "the model gateway"},
	{Label: "mail", Why: "the cluster's mail host"},
	{Label: "mail-egress", Why: "the address the cluster's mail leaves from"},
	{Label: gentianov1alpha1.PlatformTenantName, Why: "the platform administrator's desktop, and the platform tenant's zone below it"},
	{Label: "www", Why: "an alias of the cluster's main address"},
}

// PlatformLabels is the tier reserved in every tenant.
func PlatformLabels() []Reserved { return append([]Reserved(nil), platformLabels...) }

// KernelLabels is the tier reserved where a tenant's domain is the cluster's.
func KernelLabels() []Reserved { return append([]Reserved(nil), kernelLabels...) }

// Names lists a tier's labels, for a message.
func Names(tier []Reserved) string {
	names := make([]string, 0, len(tier))
	for _, r := range tier {
		names = append(names, r.Label)
	}
	return strings.Join(names, ", ")
}

// match is the entry of a tier a label would take, or nil: the name itself,
// in any case, or anything below it (x.admin is under admin, and is the
// administration console's as much as admin is).
func match(tier []Reserved, label string) *Reserved {
	label = strings.ToLower(strings.TrimSpace(label))
	for i := range tier {
		if label == tier[i].Label || strings.HasSuffix(label, "."+tier[i].Label) {
			return &tier[i]
		}
	}
	return nil
}

// PlatformShipped reports a profile the platform's own chart ships, as
// opposed to one that reached the cluster from a catalogue.
//
// A profile reaches a cluster in two ways: rendered by the platform's chart,
// or committed to the cluster's catalogue directory by the director, which
// materialises what a catalogue serves and records on it where it came from
// (profilebundle.OriginAnnotation) and the bundle it was (profilebundle.
// Annotation). A source that states either itself is refused, so a catalogue
// cannot serve a profile that arrives without them. A profile with neither
// is one no catalogue brought. (The installer writes such profiles too, into
// the same directory, on the authority of whoever installs the cluster.)
//
// It is one of two things asked. The other is the name: a reserved label is
// admitted only for the profile named as its owner, and those names are the
// ones the director refuses to materialise from any catalogue
// (gitops.PlatformProfile), ComponentProfiles being cluster-scoped.
//
// What a profile says about itself is not asked: trustTier platform is a
// field any catalogue can write, and catalogue profiles do.
func PlatformShipped(profile *gentianov1alpha1.ComponentProfile) bool {
	if profile == nil {
		return false
	}
	return strings.TrimSpace(profile.Annotations[profilebundle.OriginAnnotation]) == "" &&
		strings.TrimSpace(profile.Annotations[profilebundle.Annotation]) == ""
}

// Zone is what the check has to know about where a component is published.
type Zone struct {
	// OnClusterDomain says the tenant's hosts are directly under the
	// cluster's domain: the user tenant of a single-tenancy cluster.
	OnClusterDomain bool
	// Domain is the tenant's domain, for the message; empty where the
	// caller does not know it.
	Domain string
}

// Refusal is an exposure that would answer on a reserved name.
type Refusal struct {
	// Exposure is the profile's entry.
	Exposure string
	// Label is the name it asked for.
	Label string
	// Reserved is the entry of the list it would take.
	Reserved Reserved
	// Kernel says it is refused as the kernel's own address under the
	// cluster's domain, rather than as a name kept in every tenant.
	Kernel bool
	domain string
}

// Message says what was refused and why, for a condition, an event or an
// HTTP answer. It does not say what was done about it; the caller does.
func (r *Refusal) Message() string {
	domain := r.domain
	if domain == "" {
		domain = "<the tenant's domain>"
	}
	if r.Kernel {
		return fmt.Sprintf(
			"exposure %q would answer on %s.%s, and %s.%s is the platform's own address on this cluster (%s): "+
				"its tenancy mode is single, so this tenant's hosts are directly under the cluster's domain. "+
				"The names a component may not take here are: %s",
			r.Exposure, r.Label, domain, r.Reserved.Label, domain, r.Reserved.Why, Names(kernelLabels))
	}
	who := "nothing may take it"
	if r.Reserved.Owner != "" {
		who = fmt.Sprintf("only the platform's own component for it, %s, may take it", r.Reserved.Owner)
	}
	return fmt.Sprintf(
		"exposure %q would answer on %s.%s, and %s is an address name the platform keeps in every tenant (%s): %s. "+
			"Give the exposure another subDomain. The names an app may not take are: %s",
		r.Exposure, r.Label, domain, r.Reserved.Label, r.Reserved.Why, who, Names(platformLabels))
}

// Check answers the first entry of a profile that would answer on a reserved
// name were it installed as component in zone, or nil.
//
// Every entry that is published under a label is looked at, on either
// surface, by the label its host is built from: its subDomain, or the
// component's name. Two kinds are not: an apex entry, which has no label and
// its own rule, and an entry another component serves (an add-on's, routed
// by its base on the base's host), which is that component's to answer for.
//
// A reserved platform name is admitted for one claimant: the profile the
// platform ships for it, installed under its own name. The desktop may be at
// desktop and not at admin; and a profile called desktop that came from a
// catalogue is not the desktop.
func Check(component string, profile *gentianov1alpha1.ComponentProfile, zone Zone) *Refusal {
	if profile == nil {
		return nil
	}
	shipped := PlatformShipped(profile)
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Apex || (e.Backend.Component != "" && e.Backend.Component != component) {
			continue
		}
		label := e.SubDomain
		if label == "" {
			label = component
		}
		// The kernel's own addresses first, where the tenant is at their
		// level: nothing is admitted to them, and the reason given is the
		// one that is true there.
		if zone.OnClusterDomain {
			if reserved := match(kernelLabels, label); reserved != nil {
				return &Refusal{Exposure: e.Name, Label: label, Reserved: *reserved, Kernel: true, domain: zone.Domain}
			}
		}
		if reserved := match(platformLabels, label); reserved != nil {
			own := shipped && reserved.Owner != "" && profile.Name == reserved.Owner && label == reserved.Label
			if !own {
				return &Refusal{Exposure: e.Name, Label: label, Reserved: *reserved, domain: zone.Domain}
			}
		}
	}
	return nil
}
