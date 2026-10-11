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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// The director shows a perimeter approver the address an entry would be
// published at (addresses.Resolve). The operator publishes it in two places:
// a listener for the host (publishedHost, and the main address's holder) and
// a route behind it (livePerimeterExposures, given what mainAddressFor
// decided). The address shown is the address published, for every kind of
// tenant and entry -- or the approver approves one thing and gets another.
func TestTheAddressShownToAnApproverIsTheAddressPublished(t *testing.T) {
	const kd = "k.example"
	now := time.Now()
	published := metav1.NewTime(now.Add(-time.Hour))

	profile := func(name string, entry gentianov1alpha1.ExposureSpec) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{}
		p.Name = name
		p.Annotations = map[string]string{profilebundle.OriginAnnotation: "cluster/main"}
		entry.Surface, entry.AuthMode = gentianov1alpha1.SurfacePerimeter, gentianov1alpha1.AuthModeNone
		p.Spec.Expose = []gentianov1alpha1.ExposureSpec{entry}
		return p
	}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"cloud":   profile("cloud", gentianov1alpha1.ExposureSpec{Name: "shares", SubDomain: "share", Paths: []string{"/s/"}}),
		"notes":   profile("notes", gentianov1alpha1.ExposureSpec{Name: "pub", Paths: []string{"/p/"}}),
		"website": profile("website", gentianov1alpha1.ExposureSpec{Name: "site", Apex: true, Paths: []string{"/"}}),
		"blog":    profile("blog", gentianov1alpha1.ExposureSpec{Name: "site", Apex: true, Paths: []string{"/"}}),
		"taken":   profile("taken", gentianov1alpha1.ExposureSpec{Name: "site", SubDomain: "login", Paths: []string{"/"}}),
		"silent":  profile("silent", gentianov1alpha1.ExposureSpec{Name: "none", SubDomain: "quiet"}),
	}

	ready := func(tenant *gentianov1alpha1.Tenant, domain string) *gentianov1alpha1.Tenant {
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseReady
		tenant.Status.Domain = domain
		return tenant
	}
	tenants := map[string]func() *gentianov1alpha1.Tenant{
		"a tenant":                    func() *gentianov1alpha1.Tenant { return ready(acmeTenantFixture(), "") },
		"a tenant on its own domain":  func() *gentianov1alpha1.Tenant { return ready(acmeTenantFixture(), "acme.example") },
		"the user tenant":             func() *gentianov1alpha1.Tenant { return ready(singleUserTenantFixture(), "") },
		"the user tenant, own domain": func() *gentianov1alpha1.Tenant { return ready(singleUserTenantFixture(), "user.example") },
		"the platform tenant":         func() *gentianov1alpha1.Tenant { return ready(platformTenantFixture(), "") },
		"the user tenant, not Ready": func() *gentianov1alpha1.Tenant {
			t := ready(singleUserTenantFixture(), "")
			t.Status.Phase = ""
			return t
		},
		"the user tenant, blog first": func() *gentianov1alpha1.Tenant { return ready(singleUserTenantFixture(), "") },
	}

	checked, somewhere := 0, 0
	for what, tenantOf := range tenants {
		for _, mode := range []string{"multi", "single"} {
			for install, p := range profiles {
				for _, saysApex := range []bool{false, true} {
					entry := &p.Spec.Expose[0]
					tenant := tenantOf()
					if what == "the user tenant, blog first" && install != "blog" {
						earlier := metav1.NewTime(now.Add(-48 * time.Hour))
						tenant.Spec.Exposures = append(tenant.Spec.Exposures, gentianov1alpha1.TenantExposure{
							Install: "blog", ExposureName: "site", Apex: true, PublishedAt: &earlier,
						})
					}
					tenant.Spec.Exposures = append(tenant.Spec.Exposures, gentianov1alpha1.TenantExposure{
						Install: install, ExposureName: entry.Name, Apex: saysApex, PublishedAt: &published,
					})
					in := mainAddressInputs{
						Tenants: []gentianov1alpha1.Tenant{*tenant}, Profiles: profiles,
						KernelDomain: kd, KernelRealm: "kernel", TenancyMode: mode, Now: now,
					}
					name := fmt.Sprintf("%s, %s, %s/%s, approved apex=%v", what, mode, install, entry.Name, saysApex)

					// The operator: the component is held whole for a
					// reserved name; otherwise the route's host.
					zone := zoneNamesOf(tenant, kd, mode, "kernel")
					comp := componentFor(install, tenantNamespaceName(tenant))
					comp.Spec.Exposures = tenantExposures(tenant, install)
					route := ""
					if reservedHostRefusal(comp, p, zone, kd) == "" {
						main := perimeterMainAddress{held: mainAddressHolder(in) != nil}
						if !tenantAdoptsKernelRealm(tenant, "kernel") {
							if verdict, applies := mainAddressVerdictFor(in, tenant, install); applies && verdict.Entry != "" {
								main.entry, main.host = verdict.Entry, kd
							}
						}
						live := livePerimeterExposures(comp, p, edgeZone{zoneNames: zone}, now, main)
						if len(live) == 1 && len(perimeterPrefixes(live[0].spec)) > 0 {
							route = live[0].host
						}
					}

					shown, why := addresses.Resolve(in, &in.Tenants[0], install, entry, saysApex)
					if shown != route {
						t.Errorf("%s: the approver is shown %q and the operator publishes at %q", name, shown, route)
					}
					if (shown == "") != (why != "") {
						t.Errorf("%s: host %q with the reason %q; an entry published nowhere says why, and no other does", name, shown, why)
					}
					// A route needs a listener for the same host.
					if route != "" {
						listener := publishedHost(&tenant.Spec.Exposures[len(tenant.Spec.Exposures)-1], profiles, zone, kd)
						if holder := mainAddressHolder(in); listener == "" && holder != nil && holder.Install == install {
							listener = kd
						}
						if listener != route {
							t.Errorf("%s: the route is for %q and the listener for %q", name, route, listener)
						}
						somewhere++
					}
					checked++
				}
			}
		}
	}
	// The table has to reach both answers, or it holds nothing.
	if somewhere < 20 || checked-somewhere < 20 {
		t.Fatalf("%d cases, %d published somewhere: the table no longer covers both", checked, somewhere)
	}
}
