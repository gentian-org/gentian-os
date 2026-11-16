/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package addresses

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// A website on the cluster's main address (routing.md §5).
//
// The main address is the cluster's bare domain. One tenant may put a public
// website there: the one the cluster names (spec.mainAddress.tenant on the
// Cluster claim), under either tenancy mode, and where it names none, the
// user tenant of a single-tenancy cluster. A multi-tenancy cluster that names
// none keeps its main address for the sign-in form. It is the most visible
// page of the cluster and nobody is signed in on it, so every condition is
// explicit and all of them must hold:
//
//   - the tenant is the one the cluster names, or, where it names none, the
//     user tenant of a single-tenancy cluster on the cluster's own domain;
//   - the profile's entry is a perimeter one that says apex;
//   - the tenant's perimeter approver published it and said apex too;
//   - it has not expired, and it stays off the paths the platform keeps;
//   - the tenant is Ready, and no earlier surface already holds the address.
//
// Three writers have to agree on who holds it: the website's own component
// (its route), the platform tenant's component on the bare domain (which
// steps back to the platform's paths) and the kernel's front door (which
// stops sending the front page to the desktop). They all ask
// Holder, a pure function of the tenants, the profiles and the
// mode, so they come to the same answer.

// The platform's paths on the main address. A website never serves them.
//
//	/branding/                     the brand files every console loads
//	/sign-in                       always leads to sign-in
//	/.well-known/acme-challenge/   answering it proves control of the domain
//	/.well-known/pki-validation/   to a certificate authority; so does this
//
// Everything else under /.well-known/ is the website's.
const (
	BrandingPrefix = "/branding/"
	SignInPrefix   = "/sign-in"
)

// ReservedPrefixes are the platform's paths on the main address.
var ReservedPrefixes = []string{
	BrandingPrefix,
	SignInPrefix,
	"/.well-known/acme-challenge/",
	"/.well-known/pki-validation/",
}

// PlatformPrefixes are the reserved paths the platform's own page
// is still routed for while a website holds the address. /sign-in is not
// among them: the front door redirects it to the desktop.
func PlatformPrefixes() []string {
	var out []string
	for _, p := range ReservedPrefixes {
		if p != SignInPrefix {
			out = append(out, p)
		}
	}
	return out
}

// PathWithin reports a path prefix that lies inside another: equal to it, or
// below it by whole segments.
func PathWithin(path, prefix string) bool {
	path, prefix = strings.TrimSuffix(path, "/"), strings.TrimSuffix(prefix, "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// reservedPath is the platform's path a declared prefix would be
// inside, or "". A prefix above a reserved one (/ itself, /.well-known/) is
// fine: the reserved path is longer and so outranks it at the Gateway.
func reservedPath(e *gentianov1alpha1.ExposureSpec) string {
	for _, p := range Prefixes(e) {
		for _, reserved := range ReservedPrefixes {
			if PathWithin(p, reserved) {
				return reserved
			}
		}
	}
	return ""
}

// Claim is one surface a tenant's approver published for a bare name.
type Claim struct {
	Tenant, Install, Exposure string
	// Host is the bare name it holds: the cluster's main address or the
	// tenant's own (Inputs.BareName).
	Host string
}

func (c Claim) String() string { return c.Install + "/" + c.Exposure }

// Inputs is what decides who holds the main address.
type Inputs struct {
	Tenants      []gentianov1alpha1.Tenant
	Profiles     map[string]*gentianov1alpha1.ComponentProfile
	KernelDomain string
	KernelRealm  string
	TenancyMode  string
	// MainAddressTenant is the tenant the cluster names for its main address
	// (spec.mainAddress.tenant on the Cluster claim), under either tenancy
	// mode. Empty is the default of the mode: the user tenant on a
	// single-tenancy cluster, and nobody on a multi-tenancy one, whose main
	// address is then the sign-in form.
	MainAddressTenant string
	Now               time.Time
}

// MainTenantName is the name of the tenant that may hold the main address,
// or "" where the cluster has none: the one it names, else the user tenant
// of a single-tenancy cluster.
func (in Inputs) MainTenantName() string {
	return tenancy.MainAddressTenant(in.TenancyMode, in.MainAddressTenant)
}

// MainTenant is the tenant that may hold the main address, or nil: the one
// MainTenantName names, where it exists, is not being deleted and has a realm
// of its own. Never the tenant that adopts the kernel realm: the accounts of
// that realm administer the cluster, and its own page is the sign-in form.
func (in Inputs) MainTenant() *gentianov1alpha1.Tenant {
	name := in.MainTenantName()
	if name == "" {
		return nil
	}
	for i := range in.Tenants {
		t := &in.Tenants[i]
		if t.DeletionTimestamp == nil && t.Name == name && !AdoptsKernelRealm(t, in.KernelRealm) {
			return t
		}
	}
	return nil
}

// ApexEntry is the profile's perimeter entry of this name that says apex.
func ApexEntry(profile *gentianov1alpha1.ComponentProfile, name string) *gentianov1alpha1.ExposureSpec {
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

// BareName is where a tenant's apex entry answers, or "" where the tenant
// has no bare name of its own to give a website.
//
//   - The tenant that holds the cluster's main address (MainTenant): the
//     cluster's bare domain. That tenant is the cluster administrator's
//     choice (mainAddress.tenant), or the user tenant of a single-tenancy
//     cluster, so a website there always has the administrator's word.
//   - The user tenant of a single-tenancy cluster that names nobody, once a
//     domain of its own is bound to it: that domain. The cluster's domain is
//     then no longer its address.
//   - Any other tenant: its own domain's bare name, the zone under the
//     cluster's domain (team.<kernel>) or a domain bound to it (acme.example).
//   - Nobody else. Not the platform tenant, whose page is on the bare domain
//     either way, and not a tenant whose domain is the cluster's own without
//     the cluster naming it: that is the cluster's address, not the tenant's.
func (in Inputs) BareName(tenant *gentianov1alpha1.Tenant) string {
	if tenant == nil || AdoptsKernelRealm(tenant, in.KernelRealm) {
		return ""
	}
	domain := tenant.EffectiveDomain(in.KernelDomain, in.TenancyMode)
	if main := in.MainTenant(); main != nil && main.Name == tenant.Name {
		if in.MainAddressTenant == "" && domain != "" && !onClusterDomain(domain, in.KernelDomain) {
			return domain
		}
		return in.KernelDomain
	}
	if domain == "" || onClusterDomain(domain, in.KernelDomain) {
		return ""
	}
	return domain
}

// onMainAddress says a bare name is the cluster's main address.
func (in Inputs) onMainAddress(host string) bool {
	return host != "" && host == in.KernelDomain
}

// whereLabel is how the messages name a bare name: the cluster's main address,
// or the tenant's own.
func (in Inputs) whereLabel(host string) string {
	if host == "" || in.onMainAddress(host) {
		return "the cluster's main address"
	}
	return "the tenant's bare name " + host
}

// Refusal is why one published surface is not on its tenant's bare name, as
// a reason and a sentence for the tenant's administrator; both empty when
// nothing but another claim could stand in its way.
func (in Inputs) Refusal(tenant *gentianov1alpha1.Tenant, e *gentianov1alpha1.TenantExposure) (reason, message string) {
	host := in.BareName(tenant)
	where := in.whereLabel(host)
	entry := ApexEntry(in.Profiles[e.Install], e.ExposureName)
	switch {
	case entry == nil:
		return "NotAMainAddressEntry", fmt.Sprintf(
			"%s/%s was published for a bare name, but the profile does not declare it as a perimeter entry with apex: true. Nothing is published",
			e.Install, e.ExposureName)
	case !entry.ApprovedAs(e.Kind):
		return "ApprovedAsAnotherKind", fmt.Sprintf(
			"%s/%s was approved as another kind of entry than its profile declares now, and that approval does not cover it. Nothing is published",
			e.Install, e.ExposureName)
	case !e.Apex:
		return "NotRequested", fmt.Sprintf(
			"%s/%s is declared for a bare name and is published only when the approver says so (apex: true on the request). Nothing is published",
			e.Install, e.ExposureName)
	case host == "" && in.MainAddressTenant != "":
		return "NotTheMainAddressTenant", fmt.Sprintf(
			"this tenant's address is the cluster's own domain, and the cluster names the tenant %q for its main address (mainAddress.tenant on the Cluster claim, the cluster administrator's setting). Only that tenant may put a website there. Nothing is published",
			in.MainAddressTenant)
	case host == "":
		return "NoBareName", "this tenant's address is the cluster's own domain, and the cluster does not name it for its main address: that address is the cluster administrator's to give (mainAddress.tenant on the Cluster claim). Nothing is published"
	case e.ExpiresAt != nil && !in.Now.Before(e.ExpiresAt.Time):
		return "Expired", fmt.Sprintf("the approval of %s/%s for %s has expired. Nothing is published", e.Install, e.ExposureName, where)
	case len(Prefixes(entry)) == 0:
		return "NoPaths", fmt.Sprintf("%s/%s declares no paths, so it publishes nothing", e.Install, e.ExposureName)
	}
	if reserved := reservedPath(entry); reserved != "" {
		return "ReservedPath", fmt.Sprintf(
			"%s/%s declares a path inside %s, which the platform keeps on every bare name (%s). Nothing is published",
			e.Install, e.ExposureName, reserved, strings.Join(ReservedPrefixes, ", "))
	}
	if tenant.Status.Phase != gentianov1alpha1.TenantPhaseReady {
		return "TenantNotReady", "the tenant is not Ready yet; " + where + " changes once it is"
	}
	return "", ""
}

// Holder is the surface that holds the cluster's main address, or
// nil when none does.
func Holder(in Inputs) *Claim {
	tenant := in.MainTenant()
	if tenant == nil || !in.onMainAddress(in.BareName(tenant)) {
		return nil
	}
	return HolderOf(in, tenant)
}

// HolderOf is the surface that holds a tenant's bare name (BareName), or nil
// when none does.
//
// One at a time. Where several were published for it, the one published
// first holds it and keeps it: a later request cannot take a running website
// off the address.
func HolderOf(in Inputs, tenant *gentianov1alpha1.Tenant) *Claim {
	if tenant == nil || tenant.DeletionTimestamp != nil {
		return nil
	}
	host := in.BareName(tenant)
	if host == "" {
		return nil
	}
	type candidate struct {
		claim Claim
		at    *metav1.Time
	}
	var live []candidate
	for i := range tenant.Spec.Exposures {
		e := &tenant.Spec.Exposures[i]
		if !e.Apex {
			continue
		}
		if reason, _ := in.Refusal(tenant, e); reason != "" {
			continue
		}
		live = append(live, candidate{
			claim: Claim{Tenant: tenant.Name, Install: e.Install, Exposure: e.ExposureName, Host: host},
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

// BareNameHolders are the surfaces that hold a tenant's own bare name, every
// tenant's but the main address's holder, by tenant name.
func BareNameHolders(in Inputs) map[string]*Claim {
	out := map[string]*Claim{}
	for i := range in.Tenants {
		t := &in.Tenants[i]
		if c := HolderOf(in, t); c != nil && !in.onMainAddress(c.Host) {
			out[t.Name] = c
		}
	}
	return out
}

// Verdict is what the tenant's administrator reads on a component
// one of whose surfaces is for a bare name.
type Verdict struct {
	// Entry is the profile entry that holds the bare name, and Host that
	// name; both empty when none of this component's does.
	Entry, Host     string
	Reason, Message string
}

// VerdictFor decides for one component of one tenant. applies is
// false for a component that has nothing to do with a bare name.
func VerdictFor(in Inputs, tenant *gentianov1alpha1.Tenant, install string) (verdict Verdict, applies bool) {
	holder := HolderOf(in, tenant)
	host := in.BareName(tenant)
	for i := range tenant.Spec.Exposures {
		e := &tenant.Spec.Exposures[i]
		if e.Install != install {
			continue
		}
		if !e.Apex && ApexEntry(in.Profiles[install], e.ExposureName) == nil {
			continue
		}
		if reason, message := in.Refusal(tenant, e); reason != "" {
			verdict, applies = Verdict{Reason: reason, Message: message}, true
			continue
		}
		if holder != nil && holder.Install == install && holder.Exposure == e.ExposureName {
			return Verdict{
				Entry:  e.ExposureName,
				Host:   host,
				Reason: "Published",
				Message: fmt.Sprintf(
					"%s/%s is published on %s, https://%s/. It shows there once its proxy is up; %s on that address stay the platform's, and https://%s%s always leads to sign-in",
					install, e.ExposureName, in.whereLabel(host), host, strings.Join(ReservedPrefixes, ", "),
					host, SignInPrefix),
			}, true
		}
		verdict, applies = Verdict{
			Reason: "HeldByAnother",
			Message: fmt.Sprintf(
				"%s is already held by %s, and one surface holds it at a time. Withdraw that one first. Nothing is published",
				in.whereLabel(host), holder),
		}, true
	}
	return verdict, applies
}
