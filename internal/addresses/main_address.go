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

// Claim is one surface a tenant's approver published for the main address.
type Claim struct {
	Tenant, Install, Exposure string
}

func (c Claim) String() string { return c.Install + "/" + c.Exposure }

// Inputs is what decides who holds the main address.
type Inputs struct {
	Tenants      []gentianov1alpha1.Tenant
	Profiles     map[string]*gentianov1alpha1.ComponentProfile
	KernelDomain string
	KernelRealm  string
	TenancyMode  string
	Now          time.Time
}

// UserTenant is the one user tenant of a single-tenancy cluster, or nil.
func (in Inputs) UserTenant() *gentianov1alpha1.Tenant {
	if gentianov1alpha1.NormalizeTenancyMode(in.TenancyMode) != gentianov1alpha1.TenancyModeSingle {
		return nil
	}
	for i := range in.Tenants {
		t := &in.Tenants[i]
		if t.DeletionTimestamp == nil && t.Name == gentianov1alpha1.SingleUserTenantName &&
			!AdoptsKernelRealm(t, in.KernelRealm) {
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

// Refusal is why one published surface is not on the main address, as a reason and a sentence for the tenant's administrator; both
// empty when nothing but another claim could stand in its way.
func (in Inputs) Refusal(tenant *gentianov1alpha1.Tenant, e *gentianov1alpha1.TenantExposure) (reason, message string) {
	entry := ApexEntry(in.Profiles[e.Install], e.ExposureName)
	switch {
	case entry == nil:
		return "NotAMainAddressEntry", fmt.Sprintf(
			"%s/%s was published for the cluster's main address, but the profile does not declare it as a perimeter entry with apex: true. Nothing is published",
			e.Install, e.ExposureName)
	case !entry.ApprovedAs(e.Kind):
		return "ApprovedAsAnotherKind", fmt.Sprintf(
			"%s/%s was approved as another kind of entry than its profile declares now, and that approval does not cover it. Nothing is published",
			e.Install, e.ExposureName)
	case !e.Apex:
		return "NotRequested", fmt.Sprintf(
			"%s/%s is declared for the cluster's main address and is published only when the approver says so (apex: true on the request). Nothing is published",
			e.Install, e.ExposureName)
	case gentianov1alpha1.NormalizeTenancyMode(in.TenancyMode) != gentianov1alpha1.TenancyModeSingle:
		return "MultiTenancy", "this cluster's tenancy mode is multi: its main address is the sign-in form and no tenant's website. Nothing is published"
	case in.UserTenant() == nil || in.UserTenant().Name != tenant.Name:
		return "NotTheUserTenant", fmt.Sprintf(
			"only the user tenant %q may put a website on the cluster's main address. Nothing is published",
			gentianov1alpha1.SingleUserTenantName)
	case !onClusterDomain(tenant.EffectiveDomain(in.KernelDomain, in.TenancyMode), in.KernelDomain):
		return "OwnDomain", "this tenant is on a domain of its own, so the cluster's main address is not its address. Nothing is published"
	case e.ExpiresAt != nil && !in.Now.Before(e.ExpiresAt.Time):
		return "Expired", fmt.Sprintf("the approval of %s/%s for the main address has expired. Nothing is published", e.Install, e.ExposureName)
	case len(Prefixes(entry)) == 0:
		return "NoPaths", fmt.Sprintf("%s/%s declares no paths, so it publishes nothing", e.Install, e.ExposureName)
	}
	if reserved := reservedPath(entry); reserved != "" {
		return "ReservedPath", fmt.Sprintf(
			"%s/%s declares a path inside %s, which the platform keeps on the main address (%s). Nothing is published",
			e.Install, e.ExposureName, reserved, strings.Join(ReservedPrefixes, ", "))
	}
	if tenant.Status.Phase != gentianov1alpha1.TenantPhaseReady {
		return "TenantNotReady", "the tenant is not Ready yet; the main address changes once it is"
	}
	return "", ""
}

// Holder is the surface that holds the cluster's main address, or
// nil when none does.
//
// One at a time. Where several were published for it, the one published
// first holds it and keeps it: a later request cannot take a running website
// off the address.
func Holder(in Inputs) *Claim {
	tenant := in.UserTenant()
	if tenant == nil {
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
			claim: Claim{Tenant: tenant.Name, Install: e.Install, Exposure: e.ExposureName},
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

// Verdict is what the tenant's administrator reads on a component
// one of whose surfaces is for the main address.
type Verdict struct {
	// Entry is the profile entry that holds the main address; empty when
	// none of this component's does.
	Entry           string
	Reason, Message string
}

// VerdictFor decides for one component of one tenant. applies is
// false for a component that has nothing to do with the main address.
func VerdictFor(in Inputs, tenant *gentianov1alpha1.Tenant, install string) (verdict Verdict, applies bool) {
	holder := Holder(in)
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
		if holder != nil && holder.Tenant == tenant.Name && holder.Install == install && holder.Exposure == e.ExposureName {
			return Verdict{
				Entry:  e.ExposureName,
				Reason: "Published",
				Message: fmt.Sprintf(
					"%s/%s is published on the cluster's main address, https://%s/. The front page shows it once its proxy is up; %s on that address stay the platform's, and https://%s%s always leads to sign-in",
					install, e.ExposureName, in.KernelDomain, strings.Join(ReservedPrefixes, ", "),
					in.KernelDomain, SignInPrefix),
			}, true
		}
		verdict, applies = Verdict{
			Reason: "HeldByAnother",
			Message: fmt.Sprintf(
				"the cluster's main address is already held by %s, and one surface holds it at a time. Withdraw that one first. Nothing is published",
				holder),
		}, true
	}
	return verdict, applies
}
