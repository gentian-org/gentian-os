/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
)

// The bare name of a tenant's address: what it answers with and where it
// leads (routing.md §5).

// Which bare names no certificate of the cluster covers: a domain of the
// tenant's own. The cluster's certificate names <kernel> and *.<kernel>.
func TestOwnDomainApex(t *testing.T) {
	for domain, want := range map[string]bool{
		"acme.example":       true,
		"shop.acme.example":  true,
		"k.example":          false, // the cluster's own
		"acme.k.example":     false, // a tenant's zone under it
		"platform.k.example": false,
		"":                   false,
	} {
		if got := ownDomainApex(domain, "k.example"); got != want {
			t.Errorf("ownDomainApex(%q) = %v, want %v", domain, got, want)
		}
	}
}

// The bare name of a domain of its own gets a certificate naming it alone,
// and a listener for exactly that name on the perimeter, which serves it.
// Without them the cluster's catch-all answers with the cluster's
// certificate, and the browser refuses.
func TestTheBareNameOfAnOwnDomainHasItsOwnCertificateAndListener(t *testing.T) {
	own := acmeTenantFixture()
	own.Status.Domain = "acme.example"
	zoned := acmeTenantFixture()
	zoned.Name = "team"

	cert := buildTenantApexCertificate(own, "tenant-acme", "acme.example", "issuer")
	names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if len(names) != 1 || names[0] != "acme.example" {
		t.Fatalf("apex certificate names %v, want exactly [acme.example]", names)
	}
	if secret, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName"); secret != tenantApexSecretName("acme") {
		t.Fatalf("apex certificate secret %q", secret)
	}

	gw := buildPerimeterGateway("k.example", "multi", "", "kernel", []gentianov1alpha1.Tenant{*own, *zoned}, perimeterProfiles())
	var found bool
	for _, l := range gw.Spec.Listeners {
		if l.Hostname == nil {
			continue
		}
		switch string(*l.Hostname) {
		case "acme.example":
			found = true
			ref := l.TLS.CertificateRefs[0]
			if string(ref.Name) != "tenant-acme-apex-tls" || ref.Namespace == nil || string(*ref.Namespace) != "tenant-acme" {
				t.Fatalf("the bare name is served with %s/%s", ptrString(ref.Namespace), ref.Name)
			}
			if string(l.Name) != perimeterListenerName("acme.example") {
				t.Fatalf("listener name %q", l.Name)
			}
		case "team.k.example":
			t.Fatal("a zone under the cluster's domain got a listener: the cluster's certificate already names it")
		}
	}
	if !found {
		t.Fatal("the bare name of a domain of the tenant's own has no listener")
	}
}

// Its redirect attaches to that listener; a zone's stays on the catch-all,
// where the cluster's certificate names it.
func TestTheApexRouteOfAnOwnDomainIsOnThePerimeter(t *testing.T) {
	specs := kernelHTTPRouteSpecs("k.example", []string{"acme.example", "team.k.example"}, nil,
		[]string{"acme", "team"}, kernelConsoles{}, "c1", true, true, kernelFrontDoor{})
	byName := map[string]kernelHTTPRouteSpec{}
	for _, s := range specs {
		byName[s.name] = s
	}
	acme := buildKernelHTTPRoute(byName["tenant-acme-apex"])
	if p := acme.Spec.ParentRefs[0]; string(p.Name) != PerimeterGatewayName || p.SectionName == nil || string(*p.SectionName) != perimeterListenerName("acme.example") {
		t.Fatalf("acme.example's apex attaches to %+v", p)
	}
	team := buildKernelHTTPRoute(byName["tenant-team-apex"])
	if p := team.Spec.ParentRefs[0]; string(p.Name) != AuthenticatedGatewayName || string(*p.SectionName) != wildcardListenerName {
		t.Fatalf("team.k.example's apex attaches to %+v", p)
	}
}

// A tenant that publishes a site at www has its bare name lead there, path
// kept; one whose approval has expired leads back to its desktop; a home the
// tenant chose wins over it.
func TestTheBareNameLeadsToTheSiteAtWWW(t *testing.T) {
	site := &gentianov1alpha1.ComponentProfile{}
	site.Name = "site"
	site.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name: "web", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone,
		SubDomain: "www", Paths: []string{"/"},
		Backend: gentianov1alpha1.BackendRef{Service: "site", Port: 8080},
	}}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"site": site}
	tenant := func(name, domain string, expires time.Time, home string) gentianov1alpha1.Tenant {
		t := acmeTenantFixture()
		t.Name = name
		t.Status.Domain = domain
		t.Spec.Exposures = []gentianov1alpha1.TenantExposure{{
			Install: "site", ExposureName: "web", Owner: "u-1", ExpiresAt: exposureEnds(expires),
		}}
		if home != "" {
			t.Spec.SignUp = &gentianov1alpha1.TenantSignUp{Enabled: true, Home: home}
		}
		return *t
	}
	now := time.Now()
	tenants := []gentianov1alpha1.Tenant{
		tenant("acme", "acme.example", now.Add(time.Hour), ""),
		tenant("team", "", now.Add(time.Hour), ""),
		tenant("old", "old.example", now.Add(-time.Hour), ""),
		tenant("shop", "shop.example", now.Add(time.Hour), "https://shop.example/account"),
	}
	www := tenantWWW(tenants, profiles, "k.example", "multi", "kernel", now)
	if www["acme"] != "www.acme.example" || www["team"] != "www.team.k.example" || www["old"] != "" {
		t.Fatalf("www = %v", www)
	}
	domains, names := zonedTenantDomains(tenants, "k.example", "multi")
	specs := kernelHTTPRouteSpecs("k.example", domains, nil, names, kernelConsoles{}, "c1", true, true, kernelFrontDoor{})
	withTenantWWW(specs, www)
	withTenantHomes(specs, tenantHomes(tenants))
	target := map[string]string{}
	for _, s := range specs {
		if r := buildKernelHTTPRoute(s); len(r.Spec.Rules) > 0 && len(r.Spec.Rules[0].Filters) > 0 && r.Spec.Rules[0].Filters[0].RequestRedirect != nil && r.Spec.Rules[0].Filters[0].RequestRedirect.Hostname != nil {
			target[s.name] = string(*r.Spec.Rules[0].Filters[0].RequestRedirect.Hostname)
		}
	}
	for route, want := range map[string]string{
		"tenant-acme-apex": "www.acme.example",
		"tenant-team-apex": "www.team.k.example",
		"tenant-old-apex":  "desktop.old.example",
		"tenant-shop-apex": "shop.example",
	} {
		if target[route] != want {
			t.Errorf("%s leads to %q, want %q", route, target[route], want)
		}
	}
	// The path is kept: acme.example/en/about lands on www.acme.example/en/about.
	for _, s := range specs {
		if s.name == "tenant-acme-apex" {
			if p := buildKernelHTTPRoute(s).Spec.Rules[0].Filters[0].RequestRedirect.Path; p != nil {
				t.Fatalf("the redirect to www rewrites the path: %+v", p)
			}
		}
	}
}

func ptrString[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// A tenant's website on its own bare name (routing.md §5): its zone under the
// cluster's domain gets a listener with the cluster's certificate, a domain of
// its own keeps the listener it has, and the tenant's apex redirect moves onto
// that listener. Once the website's proxy serves, the redirect keeps /sign-in
// alone; until then it leads where it led before.
func TestATenantsWebsiteTakesItsOwnBareName(t *testing.T) {
	now := time.Now()
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"website": websiteProfile(t, "website")}
	team := readyTenant(acmeTenantFixture())
	team.Name = "team"
	team.Spec.Exposures = []gentianov1alpha1.TenantExposure{approved("website", now)}
	acme := readyTenant(acmeTenantFixture())
	acme.Status.Domain = "acme.example"
	acme.Spec.Exposures = []gentianov1alpha1.TenantExposure{approved("website", now)}
	idle := readyTenant(acmeTenantFixture())
	idle.Name = "idle"
	tenants := []gentianov1alpha1.Tenant{*team, *acme, *idle}

	in := mainAddressInputs{
		Tenants: tenants, Profiles: profiles,
		KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi", Now: now,
	}
	holders := addresses.BareNameHolders(in)
	if len(holders) != 2 || holders["team"].Host != "team.k.example" || holders["acme"].Host != "acme.example" {
		t.Fatalf("bare-name holders = %v", holders)
	}
	if mainAddressHolder(in) != nil {
		t.Fatal("a tenant's own bare name was taken for the cluster's main address")
	}

	gw := buildPerimeterGateway("k.example", "multi", "", "kernel", tenants, profiles)
	listeners := map[string]gatewayv1.Listener{}
	for _, l := range gw.Spec.Listeners {
		if l.Hostname != nil {
			listeners[string(*l.Hostname)] = l
		}
	}
	zone, ok := listeners["team.k.example"]
	if !ok {
		t.Fatal("the zone's bare name, held by a website, has no listener on the perimeter")
	}
	// The cluster's certificate is in the Gateway's own namespace, so the
	// reference names none.
	if ref := zone.TLS.CertificateRefs[0]; string(ref.Name) != kernelWildcardTLSSecretName || ref.Namespace != nil {
		t.Fatalf("the zone's bare name is served with %s/%s, want the cluster's certificate", ptrString(ref.Namespace), ref.Name)
	}
	if _, ok := listeners["idle.k.example"]; ok {
		t.Fatal("a zone nobody's website holds got a listener: it would take the bare name off the catch-all")
	}

	domains, names := zonedTenantDomains(tenants, "k.example", "multi")
	specs := kernelHTTPRouteSpecs("k.example", domains, nil, names, kernelConsoles{}, "c1", true, true, kernelFrontDoor{})
	withTenantBareNames(specs, map[string]tenantBareName{
		"team": {host: "team.k.example", serving: true},
		"acme": {host: "acme.example", serving: false},
	})
	byName := map[string]kernelHTTPRouteSpec{}
	for _, s := range specs {
		byName[s.name] = s
	}
	for name, host := range map[string]string{"tenant-team-apex": "team.k.example", "tenant-acme-apex": "acme.example"} {
		p := buildKernelHTTPRoute(byName[name]).Spec.ParentRefs[0]
		if string(p.Name) != PerimeterGatewayName || p.SectionName == nil || string(*p.SectionName) != perimeterListenerName(host) {
			t.Errorf("%s attaches to %+v, want the perimeter listener of %s", name, p, host)
		}
	}
	served := buildKernelHTTPRoute(byName["tenant-team-apex"]).Spec.Rules
	if len(served) != 1 || len(served[0].Matches) != 1 || served[0].Matches[0].Path == nil ||
		*served[0].Matches[0].Path.Value != mainAddressSignInPrefix ||
		string(*served[0].Filters[0].RequestRedirect.Hostname) != "desktop.team.k.example" {
		t.Fatalf("with the website serving, the zone's apex route = %+v, want /sign-in to the desktop alone", served)
	}
	waiting := buildKernelHTTPRoute(byName["tenant-acme-apex"]).Spec.Rules
	if len(waiting) != 1 || len(waiting[0].Matches) > 1 ||
		string(*waiting[0].Filters[0].RequestRedirect.Hostname) != "desktop.acme.example" {
		t.Fatalf("before the website serves, the own domain's apex route = %+v, want the redirect it had", waiting)
	}
	idleRoute := buildKernelHTTPRoute(byName["tenant-idle-apex"]).Spec.ParentRefs[0]
	if string(idleRoute.Name) != AuthenticatedGatewayName {
		t.Fatalf("a bare name nobody's website holds moved off the catch-all: %+v", idleRoute)
	}
}
