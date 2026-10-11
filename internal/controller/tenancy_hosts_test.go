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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/hostnames"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// Where everything answers, per tenancy mode. One table, because it is one
// rule (zoneNamesOf, exposureHostIn) and the routes, the perimeter listeners
// and the redirect URIs all read it.

func singleUserTenantFixture() *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{}
	t.Name = gentianov1alpha1.SingleUserTenantName
	t.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: gentianov1alpha1.SingleUserTenantName}
	return t
}

var (
	desktopEntry      = &gentianov1alpha1.ExposureSpec{Name: "web", SubDomain: "desktop"}
	adminEntry        = &gentianov1alpha1.ExposureSpec{Name: "web", SubDomain: "admin"}
	appEntry          = &gentianov1alpha1.ExposureSpec{Name: "web", SubDomain: "cloud"}
	appNamelessEntry  = &gentianov1alpha1.ExposureSpec{Name: "web"}
	conciergeApexSpec = &gentianov1alpha1.ExposureSpec{Name: "front", Apex: true, Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone}
)

func TestWhereEverythingAnswersPerTenancyMode(t *testing.T) {
	const kd = "k.example"
	cases := []struct {
		what      string
		mode      string
		tenant    *gentianov1alpha1.Tenant
		component string
		entry     *gentianov1alpha1.ExposureSpec
		want      string
	}{
		// The platform's own addresses, the same on every cluster.
		{"platform desktop, multi", "multi", platformTenantFixture(), "desktop", desktopEntry, "platform.k.example"},
		{"platform desktop, single", "single", platformTenantFixture(), "desktop", desktopEntry, "platform.k.example"},
		{"platform admin console, multi", "multi", platformTenantFixture(), "admin-console", adminEntry, "admin.platform.k.example"},
		{"platform admin console, single", "single", platformTenantFixture(), "admin-console", adminEntry, "admin.platform.k.example"},
		// The bare domain is the platform tenant's to publish on, either
		// mode: the cluster's brand is served from there.
		{"concierge, multi", "multi", platformTenantFixture(), "concierge", conciergeApexSpec, "k.example"},
		{"concierge, single", "single", platformTenantFixture(), "concierge", conciergeApexSpec, "k.example"},
		// The one user tenant of a single-tenancy cluster: the cluster's own
		// addresses, with no tenant name in between.
		{"user desktop, single", "single", singleUserTenantFixture(), "desktop", desktopEntry, "desktop.k.example"},
		{"user admin console, single", "single", singleUserTenantFixture(), "admin-console", adminEntry, "admin.k.example"},
		{"user app with a label, single", "single", singleUserTenantFixture(), "nextcloud", appEntry, "cloud.k.example"},
		{"user app by its name, single", "single", singleUserTenantFixture(), "wiki", appNamelessEntry, "wiki.k.example"},
		{"user tenant asks for the bare domain, single", "single", singleUserTenantFixture(), "x", conciergeApexSpec, ""},
		// Under multi, a tenant named user is an ordinary tenant.
		{"tenant named user, multi", "multi", singleUserTenantFixture(), "desktop", desktopEntry, "desktop.user.k.example"},
		{"a tenant, multi: desktop", "multi", acmeTenantFixture(), "desktop", desktopEntry, "desktop.acme.k.example"},
		{"a tenant, multi: admin console", "multi", acmeTenantFixture(), "admin-console", adminEntry, "admin.acme.k.example"},
		{"a tenant, multi: app", "multi", acmeTenantFixture(), "nextcloud", appEntry, "cloud.acme.k.example"},
		{"a tenant asks for the bare domain", "multi", acmeTenantFixture(), "x", conciergeApexSpec, ""},
	}
	for _, c := range cases {
		zone := zoneNamesOf(c.tenant, kd, c.mode, "kernel")
		if got := exposureHostIn(zone, c.component, c.entry); got != c.want {
			t.Errorf("%s: host = %q, want %q", c.what, got, c.want)
		}
	}
}

// The realm, the client and the listener go with the tenant, not with the
// address: the user tenant of a single-tenancy cluster is on the cluster's
// domain and in its own realm, never the kernel's.
func TestTheUserTenantOnTheClustersDomainKeepsItsOwnRealm(t *testing.T) {
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "single"}
	user := r.zoneOf(singleUserTenantFixture())
	if user.kernel || user.realm != "user" || user.clientID != "gentian-edge-user" ||
		user.secretName != "edge-user-oidc" || user.cookie != "gentian-user-access" {
		t.Fatalf("user zone = %+v", user)
	}
	if user.domain != "k.example" || user.sectionName != wildcardListenerName {
		t.Fatalf("user zone is on %q, listener %q", user.domain, user.sectionName)
	}
	platform := r.zoneOf(platformTenantFixture())
	if !platform.kernel || platform.realm != "kernel" || platform.clientID != edgeKernelClientID || platform.domain != "platform.k.example" {
		t.Fatalf("platform zone = %+v", platform)
	}
}

// A route names the listener whose certificate covers its host: the
// catch-all's for a name directly under the cluster's domain, the zone's own
// for anything deeper.
func TestARouteIsOnTheListenerWhoseCertificateNamesItsHost(t *testing.T) {
	const kd = "k.example"
	section := func(mode string, tenant *gentianov1alpha1.Tenant, component string, e *gentianov1alpha1.ExposureSpec) (string, string) {
		r := &ComponentReconciler{KernelDomain: kd, KernelRealm: "kernel", TenancyMode: mode}
		zone := r.zoneOf(tenant)
		comp := &gentianov1alpha1.Component{}
		comp.Name, comp.Namespace = component, "tenant-"+tenant.Name
		entry := *e
		entry.Surface, entry.AuthMode = gentianov1alpha1.SurfaceGateway, gentianov1alpha1.AuthModeOIDC
		entry.Backend.Service, entry.Backend.Port = "svc", 8080
		host := exposureHost(zone, comp, &entry)
		route := buildExposureRoute(comp, component+"-web", host, zone, &entry, routeAuthz{relation: "can_enter", object: "tenant:" + tenant.Name}, kd, nil)
		return string(route.Spec.Hostnames[0]), string(*route.Spec.ParentRefs[0].SectionName)
	}
	for _, c := range []struct {
		mode      string
		tenant    *gentianov1alpha1.Tenant
		component string
		entry     *gentianov1alpha1.ExposureSpec
		host      string
		listener  string
	}{
		{"multi", platformTenantFixture(), "desktop", desktopEntry, "platform.k.example", wildcardListenerName},
		{"multi", platformTenantFixture(), "admin-console", adminEntry, "admin.platform.k.example", tenantGatewayListenerName("platform")},
		{"single", platformTenantFixture(), "admin-console", adminEntry, "admin.platform.k.example", tenantGatewayListenerName("platform")},
		{"single", singleUserTenantFixture(), "desktop", desktopEntry, "desktop.k.example", wildcardListenerName},
		{"single", singleUserTenantFixture(), "admin-console", adminEntry, "admin.k.example", wildcardListenerName},
		{"multi", acmeTenantFixture(), "desktop", desktopEntry, "desktop.acme.k.example", tenantGatewayListenerName("acme")},
	} {
		host, listener := section(c.mode, c.tenant, c.component, c.entry)
		if host != c.host || listener != c.listener {
			t.Errorf("%s %s/%s: host %q on listener %q, want %q on %q", c.mode, c.tenant.Name, c.component, host, listener, c.host, c.listener)
		}
	}
}

// redirectsOf reads the front-door routes: host -> where it is sent. The
// catch-all's route for the bare domain is the one read; the perimeter's is
// checked by itself.
func redirectsOf(specs []kernelHTTPRouteSpec) map[string]string {
	out := map[string]string{}
	for _, s := range specs {
		if s.name == kernelRouteHTTPRedirect || s.name == kernelRouteApexPerimeterRedirect || len(s.rules) != 1 || len(s.rules[0].Filters) != 1 {
			continue
		}
		if rr := s.rules[0].Filters[0].RequestRedirect; rr != nil && rr.Hostname != nil {
			out[s.host] = string(*rr.Hostname)
		}
	}
	return out
}

// The bare domain, www and desktop.<kernel>, per mode.
func TestTheFrontDoorPerTenancyMode(t *testing.T) {
	const kd = "k.example"
	ready := gentianov1alpha1.TenantPhaseReady
	platform := *platformTenantFixture()
	platform.Status.Phase = ready
	user := *singleUserTenantFixture()
	user.Status.Phase = ready
	pending := *singleUserTenantFixture()
	pending.Status.Phase = gentianov1alpha1.TenantPhaseProvisioning
	custom := *singleUserTenantFixture()
	custom.Status.Phase = ready
	custom.Status.Domain = "acme.example"
	acme := *acmeTenantFixture()
	acme.Status.Phase = ready

	specsFor := func(mode string, tenants ...gentianov1alpha1.Tenant) []kernelHTTPRouteSpec {
		domains, names := zonedTenantDomains(tenants, kd, mode)
		return kernelHTTPRouteSpecs(kd, domains, nil, names, false, "c1", true, true,
			kernelFrontDoorOf(tenants, kd, "kernel", mode))
	}

	// multi: desktop.<kernel> and www lead to the bare domain, which the
	// kernel does not route -- it is the concierge's, on the perimeter.
	// A tenant named user changes nothing.
	for _, tenants := range [][]gentianov1alpha1.Tenant{{platform}, {platform, acme}, {platform, user}, {platform, user, acme}} {
		got := redirectsOf(specsFor("multi", tenants...))
		if got["www."+kd] != kd || got["desktop."+kd] != kd {
			t.Fatalf("multi, %d tenants: www -> %q, desktop -> %q, want both the bare domain", len(tenants), got["www."+kd], got["desktop."+kd])
		}
		if to, routed := got[kd]; routed {
			t.Fatalf("multi, %d tenants: the kernel routes the bare domain to %q; it is the concierge's", len(tenants), to)
		}
		if to, routed := got["platform."+kd]; routed {
			t.Fatalf("multi: platform.<kernel> is redirected to %q; it is the platform's desktop", to)
		}
	}
	// A tenant's own apex still leads to that tenant's desktop.
	if got := redirectsOf(specsFor("multi", platform, acme)); got["acme."+kd] != "desktop.acme."+kd {
		t.Fatalf("multi: acme's apex -> %q", got["acme."+kd])
	}

	// single: the bare domain and www lead to the user tenant's desktop once
	// it is Ready; desktop.<kernel> is that desktop and is not redirected.
	got := redirectsOf(specsFor("single", platform, user))
	if got[kd] != "desktop."+kd || got["www."+kd] != "desktop."+kd {
		t.Fatalf("single: bare -> %q, www -> %q, want desktop.%s", got[kd], got["www."+kd], kd)
	}
	if to, routed := got["desktop."+kd]; routed {
		t.Fatalf("single: desktop.<kernel> is redirected to %q; it is the user tenant's desktop", to)
	}
	if to, routed := got["platform."+kd]; routed {
		t.Fatalf("single: platform.<kernel> is redirected to %q", to)
	}
	// On the perimeter, where the concierge is published: only the front
	// page and the form are sent on, each to the desktop's root, so the
	// brand under /branding/ is still served from the bare domain.
	var onPerimeter *kernelHTTPRouteSpec
	for i, s := range specsFor("single", platform, user) {
		if s.name == kernelRouteApexPerimeterRedirect {
			onPerimeter = &specsFor("single", platform, user)[i]
		}
	}
	if onPerimeter == nil || onPerimeter.gateway != PerimeterGatewayName || onPerimeter.sectionName != perimeterListenerName(kd) || onPerimeter.host != kd {
		t.Fatalf("single: the bare domain's redirect on the perimeter = %+v", onPerimeter)
	}
	rule := onPerimeter.rules[0]
	var matched []string
	for _, m := range rule.Matches {
		matched = append(matched, string(*m.Path.Type)+" "+*m.Path.Value)
	}
	if strings.Join(matched, ", ") != "Exact /, PathPrefix /sign-in" {
		t.Fatalf("single: the perimeter redirect matches %v; it must leave /branding/ to the concierge", matched)
	}
	rr := rule.Filters[0].RequestRedirect
	if string(*rr.Hostname) != "desktop."+kd || rr.Path == nil || *rr.Path.ReplaceFullPath != "/" {
		t.Fatalf("single: the perimeter redirect leads to %v %v", *rr.Hostname, rr.Path)
	}
	for _, s := range specsFor("multi", platform, user) {
		if s.name == kernelRouteApexPerimeterRedirect || s.name == kernelRouteApexRedirect {
			t.Fatalf("multi: the bare domain is redirected by %s", s.name)
		}
	}
	// With a custom domain bound, the desktop is on it and the bare domain
	// follows.
	if got := redirectsOf(specsFor("single", platform, custom)); got[kd] != "desktop.acme.example" || got["www."+kd] != "desktop.acme.example" {
		t.Fatalf("single, custom domain: bare -> %q, www -> %q", got[kd], got["www."+kd])
	}
	// Before the user tenant is Ready, and before it exists: nothing claims
	// desktop.<kernel>, and the bare domain is sent nowhere yet.
	for _, tenants := range [][]gentianov1alpha1.Tenant{{platform}, {platform, pending}} {
		got := redirectsOf(specsFor("single", tenants...))
		if to, routed := got[kd]; routed {
			t.Fatalf("single, user tenant not Ready: the bare domain is sent to %q", to)
		}
		if to, routed := got["desktop."+kd]; routed {
			t.Fatalf("single, user tenant not Ready: desktop.<kernel> is redirected to %q", to)
		}
	}
	// A tenant of another name is not the user tenant, Ready or not: it is
	// refused under single and nothing is sent to it.
	if to, routed := redirectsOf(specsFor("single", platform, acme))[kd]; routed {
		t.Fatalf("single: the bare domain is sent to a tenant that is not the user tenant: %q", to)
	}
}

// The perimeter: the bare domain has a listener, with the cluster's own
// certificate, while the concierge is published on it -- under either mode,
// because the cluster's brand is served from there.
func TestTheBareDomainIsOnThePerimeterWithTheClustersCertificate(t *testing.T) {
	platform := platformTenantFixture()
	platform.Spec.Exposures = []gentianov1alpha1.TenantExposure{{
		Install: "concierge", ExposureName: "front", Owner: "install",
		ExpiresAt: exposureEnds(time.Now().Add(24 * time.Hour)),
	}}
	concierge := &gentianov1alpha1.ComponentProfile{}
	concierge.Name = "concierge"
	concierge.Spec.Expose = []gentianov1alpha1.ExposureSpec{*conciergeApexSpec}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"concierge": concierge}

	apex := func(mode string) *gatewayv1.Listener {
		gw := buildPerimeterGateway("k.example", mode, "kernel", []gentianov1alpha1.Tenant{*platform}, profiles)
		for i := range gw.Spec.Listeners {
			if h := gw.Spec.Listeners[i].Hostname; h != nil && string(*h) == "k.example" {
				return &gw.Spec.Listeners[i]
			}
		}
		return nil
	}
	for _, mode := range []string{"multi", "single"} {
		l := apex(mode)
		if l == nil {
			t.Fatalf("%s: the bare domain has no listener on the perimeter", mode)
		}
		if ref := l.TLS.CertificateRefs[0]; string(ref.Name) != kernelWildcardTLSSecretName || ref.Namespace != nil {
			t.Fatalf("%s: the bare domain's certificate = %+v, want the cluster's own", mode, ref)
		}
		if string(l.Name) != perimeterListenerName("k.example") {
			t.Fatalf("%s: listener %s", mode, l.Name)
		}
	}
}

// A component of the user tenant of a single-tenancy cluster may not take a
// name that is the kernel's; in a tenant with a domain of its own, or in the
// platform tenant, those are not the kernel's hosts and are not refused.
func TestAKernelsHostLabelIsRefusedOnTheClustersDomain(t *testing.T) {
	const kd = "k.example"
	profileWith := func(sub string) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{}
		p.Annotations = map[string]string{profilebundle.OriginAnnotation: "cluster/main"}
		p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, SubDomain: sub}}
		return p
	}
	single := zoneNamesOf(singleUserTenantFixture(), kd, "single", "kernel")
	labels := []string{"x.platform", "a.b.id"}
	for _, r := range hostnames.KernelLabels() {
		labels = append(labels, r.Label)
	}
	for _, label := range labels {
		comp := componentFor("thing", "tenant-user")
		refusal := reservedHostRefusal(comp, profileWith(label), single, kd)
		if refusal == "" {
			t.Errorf("%s.%s was not refused for the user tenant of a single-tenancy cluster", label, kd)
			continue
		}
		for _, want := range []string{label + "." + kd, "tenancy mode is single", "platform", "id", "Nothing is installed or routed"} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal of %q does not say %q: %s", label, want, refusal)
			}
		}
	}
	// By the component's name as well, where the entry names no label.
	if reservedHostRefusal(componentFor("llm", "tenant-user"), profileWith(""), single, kd) == "" {
		t.Error("a component named llm was not refused")
	}
	// Any other name is fine.
	for _, label := range []string{"cloud", "identity", "platformer", "operations"} {
		if refusal := reservedHostRefusal(componentFor("thing", "tenant-user"), profileWith(label), single, kd); refusal != "" {
			t.Errorf("%s was refused: %s", label, refusal)
		}
	}
	// An addon's entry is served by its base, on the base's host.
	addon := profileWith("llm")
	addon.Spec.Expose[0].Backend.Component = "nextcloud"
	if refusal := reservedHostRefusal(componentFor("calendar", "tenant-user"), addon, single, kd); refusal != "" {
		t.Errorf("an entry another component serves was refused: %s", refusal)
	}
	// Not asked anywhere else: a tenant with a domain of its own cannot reach
	// the kernel's names, and the platform tenant's are below platform.
	custom := acmeTenantFixture()
	custom.Status.Domain = "acme.example"
	singleCustom := singleUserTenantFixture()
	singleCustom.Status.Domain = "user.example"
	for _, zone := range []zoneNames{
		zoneNamesOf(acmeTenantFixture(), kd, "multi", "kernel"),
		zoneNamesOf(custom, kd, "multi", "kernel"),
		zoneNamesOf(singleUserTenantFixture(), kd, "multi", "kernel"),
		zoneNamesOf(singleCustom, kd, "single", "kernel"),
		zoneNamesOf(platformTenantFixture(), kd, "single", "kernel"),
		zoneNamesOf(platformTenantFixture(), kd, "multi", "kernel"),
	} {
		for _, label := range []string{"llm", "mail", "www", "argocd"} {
			if refusal := reservedHostRefusal(componentFor("thing", "tenant-x"), profileWith(label), zone, kd); refusal != "" {
				t.Errorf("zone %+v: %s refused: %s", zone, label, refusal)
			}
		}
	}
}

// An app may not take an address name the platform keeps in a tenant -- the
// desktop's, the administration console's, the App Store's, the desktop's
// former one, a sign-in page's -- in any tenant, under any domain. This was
// once the opposite for console, which an app could take.
func TestAnAppMayNotTakeThePlatformsAddressNamesInAnyTenant(t *testing.T) {
	const kd = "k.example"
	app := func(sub string) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{}
		p.Name = "thing"
		p.Annotations = map[string]string{profilebundle.OriginAnnotation: "cluster/main"}
		p.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
		p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, SubDomain: sub}}
		return p
	}
	custom := acmeTenantFixture()
	custom.Status.Domain = "acme.example"
	zones := map[string]zoneNames{
		"acme.k.example":     zoneNamesOf(acmeTenantFixture(), kd, "multi", "kernel"),
		"acme.example":       zoneNamesOf(custom, kd, "multi", "kernel"),
		"k.example":          zoneNamesOf(singleUserTenantFixture(), kd, "single", "kernel"),
		"platform.k.example": zoneNamesOf(platformTenantFixture(), kd, "multi", "kernel"),
	}
	for domain, zone := range zones {
		if zone.domain != domain {
			t.Fatalf("zone %s is at %s", domain, zone.domain)
		}
		for _, r := range hostnames.PlatformLabels() {
			refusal := reservedHostRefusal(componentFor("thing", "tenant-x"), app(r.Label), zone, kd)
			if refusal == "" {
				t.Errorf("%s: an app was admitted to %s", domain, r.Label)
				continue
			}
			if !strings.Contains(refusal, r.Label+"."+domain) || !strings.Contains(refusal, "Nothing is installed or routed") {
				t.Errorf("%s: the refusal of %s: %s", domain, r.Label, refusal)
			}
		}
		for _, label := range []string{"console", "desktop", "admin"} {
			if reservedHostRefusal(componentFor(label, "tenant-x"), app(""), zone, kd) == "" {
				t.Errorf("%s: an app named %s was admitted", domain, label)
			}
		}
	}
}

// The components the platform ships hold their own addresses in every zone
// -- rendered from the chart, so that a label changed there and not in the
// list holds every tenant's desktop in a test and not on a cluster -- and
// none holds another's. A profile of the same name a catalogue brought is
// not the platform's.
func TestThePlatformsOwnComponentsHoldTheirAddresses(t *testing.T) {
	const kd = "k.example"
	custom := acmeTenantFixture()
	custom.Status.Domain = "acme.example"
	zones := []zoneNames{
		zoneNamesOf(acmeTenantFixture(), kd, "multi", "kernel"),
		zoneNamesOf(custom, kd, "multi", "kernel"),
		zoneNamesOf(singleUserTenantFixture(), kd, "single", "kernel"),
		zoneNamesOf(platformTenantFixture(), kd, "multi", "kernel"),
		zoneNamesOf(platformTenantFixture(), kd, "single", "kernel"),
	}
	shipped := map[string]*gentianov1alpha1.ComponentProfile{
		"desktop": renderShippedProfile(t, "componentprofile-desktop.yaml",
			"desktop.enabled=true", "desktop.chart.version=0.1.0"),
		"admin-console": renderShippedProfile(t, "componentprofile-admin-console.yaml",
			"adminConsole.enabled=true", "adminConsole.chart.version=0.1.0"),
		"app-store": shippedAppStoreProfile(t),
		"concierge": renderShippedProfile(t, "componentprofile-concierge.yaml",
			"concierge.enabled=true", "concierge.chart.version=0.1.0"),
	}
	held := map[string]string{}
	for _, r := range hostnames.PlatformLabels() {
		if r.Owner != "" {
			held[r.Owner] = r.Label
			if shipped[r.Owner] == nil {
				t.Errorf("%s is held by %s, which the chart does not ship", r.Label, r.Owner)
			}
		}
	}
	for name, profile := range shipped {
		if profile.Name != name {
			t.Fatalf("the chart's %s is named %s", name, profile.Name)
		}
		if !hostnames.PlatformShipped(profile) {
			t.Errorf("the chart renders %s with a catalogue's annotations: %v", name, profile.Annotations)
		}
		// Every label it asks for is the one the list gives it.
		for i := range profile.Spec.Expose {
			e := &profile.Spec.Expose[i]
			if e.Apex {
				continue
			}
			if e.SubDomain != held[name] {
				t.Errorf("%s entry %s is at %q and the list gives it %q", name, e.Name, e.SubDomain, held[name])
			}
		}
		for _, zone := range zones {
			if refusal := reservedHostRefusal(componentFor(name, "tenant-x"), profile, zone, kd); refusal != "" {
				t.Errorf("%s is refused its own address under %s: %s", name, zone.domain, refusal)
			}
			// The same profile from a catalogue is an app called that.
			if held[name] == "" {
				continue
			}
			brought := profile.DeepCopy()
			brought.Annotations = map[string]string{profilebundle.OriginAnnotation: "tenant/acme/ours"}
			if reservedHostRefusal(componentFor(name, "tenant-x"), brought, zone, kd) == "" {
				t.Errorf("a %s from a catalogue was admitted to %s under %s", name, held[name], zone.domain)
			}
			// And it does not hold another's.
			for other, label := range held {
				if other == name {
					continue
				}
				moved := profile.DeepCopy()
				for i := range moved.Spec.Expose {
					moved.Spec.Expose[i].SubDomain = label
				}
				if reservedHostRefusal(componentFor(name, "tenant-x"), moved, zone, kd) == "" {
					t.Errorf("%s was admitted to %s, which is %s's", name, label, other)
				}
			}
		}
	}
}

// The reconciler holds a component that asks for a reserved name, whole,
// before anything is written for it, and says so once.
func TestAComponentAskingForAReservedAddressIsHeld(t *testing.T) {
	profile := materialised(t, wikiBundle)
	profile.Annotations[profilebundle.OriginAnnotation] = "cluster/main"
	profile.Spec.Expose = []gentianov1alpha1.ExposureSpec{
		{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "wiki",
			Backend: gentianov1alpha1.BackendRef{Service: "wiki", Port: 8080}},
		{Name: "manage", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "admin",
			Backend: gentianov1alpha1.BackendRef{Service: "wiki", Port: 8080}},
	}
	h := startDigestHarness(t, profile, "")
	got := h.reconcile()
	ready := componentReadyCondition(got)
	if ready == nil || ready.Status != "False" || ready.Reason != "HostReserved" {
		t.Fatalf("condition = %+v, want HostReserved", ready)
	}
	for _, want := range []string{`exposure "manage"`, "admin.acme.k.example", "administration console", "admin-console", "Nothing is installed or routed"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("the condition does not say %q: %s", want, ready.Message)
		}
	}
	// Whole: not the permitted entry either, and no release.
	if h.releasedVersion() != "" || h.networkPolicyWritten() {
		t.Fatalf("something was written for a held component: release %q, network policy %v",
			h.releasedVersion(), h.networkPolicyWritten())
	}
	routes := &gatewayv1.HTTPRouteList{}
	if err := h.c.List(context.Background(), routes); err != nil {
		t.Fatal(err)
	}
	if len(routes.Items) != 0 {
		t.Fatalf("%d route(s) were written for a held component", len(routes.Items))
	}
	if e := h.events(); len(e) != 1 || !strings.Contains(e[0], "Warning HostReserved") {
		t.Fatalf("events = %v, want one warning", e)
	}
	h.reconcile()
	if e := h.events(); len(e) != 0 {
		t.Fatalf("the refusal was repeated: %v", e)
	}
	// With the entry somewhere it may be, the component is rolled out.
	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Expose[1].SubDomain = "wiki-manage" })
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason == "HostReserved" {
		t.Fatalf("still held after the entry moved: %+v", ready)
	}
}

// A perimeter surface gets no listener at a reserved name: an exact-hostname
// listener would take the desktop's host off the authenticated Gateway.
func TestAReservedAddressGetsNoPerimeterListener(t *testing.T) {
	const kd = "k.example"
	site := func(sub string) map[string]*gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{}
		p.Name = "website"
		p.Annotations = map[string]string{profilebundle.OriginAnnotation: "cluster/main"}
		p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{Name: "site", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone, SubDomain: sub}}
		return map[string]*gentianov1alpha1.ComponentProfile{"website": p}
	}
	enabled := &gentianov1alpha1.TenantExposure{Install: "website", ExposureName: "site"}
	zone := zoneNamesOf(acmeTenantFixture(), kd, "multi", "kernel")
	if host := publishedHost(enabled, site("www"), zone, kd); host != "www.acme.k.example" {
		t.Fatalf("a tenant's website at www = %q", host)
	}
	for _, label := range []string{"desktop", "admin", "login", "console"} {
		if host := publishedHost(enabled, site(label), zone, kd); host != "" {
			t.Errorf("a perimeter surface at %s got the listener %s", label, host)
		}
	}
	single := zoneNamesOf(singleUserTenantFixture(), kd, "single", "kernel")
	if host := publishedHost(enabled, site("www"), single, kd); host != "" {
		t.Errorf("www on the cluster's domain got the listener %s", host)
	}
}

// The desktop an app may be framed by is its own tenant's, and not the
// platform's -- also when the tenant's domain is the cluster's.
func TestTheDesktopAnAppMayBeFramedBy(t *testing.T) {
	if got := strings.Join(desktopOrigins("acme.k.example"), " "); got != "https://desktop.acme.k.example" {
		t.Errorf("a tenant: %s", got)
	}
	if got := strings.Join(desktopOrigins("k.example"), " "); got != "https://desktop.k.example" {
		t.Errorf("the user tenant of a single-tenancy cluster: %s", got)
	}
	// The wildcard over the tenant's domain is only for a domain that is the
	// tenant's alone.
	if got := computeGatewayFrameAncestorsPolicy("k.example", "k.example", "").Origins; got != "https://desktop.k.example" {
		t.Errorf("the user tenant's default policy names more than its desktop: %s", got)
	}
	if got := computeGatewayFrameAncestorsPolicy("k.example", "acme.k.example", "").Origins; strings.Contains(got, "platform.") {
		t.Errorf("a tenant's default policy names the platform: %s", got)
	}
}

// Who may put a component's page in a frame, in every shape a zone has: the
// desktop of the component's own tenant and the component's own other hosts.
// Never the platform's desktop for a tenant's component, never another
// tenant, and never a wildcard that would take either in.
func TestAComponentIsFramedByItsOwnTenantsDesktopAndNobodyElses(t *testing.T) {
	const kd = "k.example"
	office := &gentianov1alpha1.ComponentProfile{}
	office.Spec.Launch = gentianov1alpha1.ComponentLaunchTile
	office.Spec.Expose = []gentianov1alpha1.ExposureSpec{
		{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "cloud",
			Backend: gentianov1alpha1.BackendRef{Service: "files", Port: 80}},
		{Name: "editor", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "editor",
			Backend: gentianov1alpha1.BackendRef{Service: "editor", Port: 9980}},
		// Somebody else's Service: not this component's host, so not a framer.
		{Name: "elsewhere", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "other",
			Backend: gentianov1alpha1.BackendRef{Component: "another", Service: "x", Port: 80}},
		// A perimeter surface has no session and no window on the desktop.
		{Name: "share", Surface: gentianov1alpha1.SurfacePerimeter, SubDomain: "share",
			Backend: gentianov1alpha1.BackendRef{Service: "files", Port: 80}},
	}
	admin := &gentianov1alpha1.ComponentProfile{}
	admin.Spec.Expose = []gentianov1alpha1.ExposureSpec{
		{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "admin",
			Backend: gentianov1alpha1.BackendRef{Service: "admin", Port: 80}},
	}
	desktop := &gentianov1alpha1.ComponentProfile{}
	desktop.Spec.Launch = gentianov1alpha1.ComponentLaunchNone
	desktop.Spec.Expose = []gentianov1alpha1.ExposureSpec{
		{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "desktop",
			Backend: gentianov1alpha1.BackendRef{Service: "desktop", Port: 80}},
	}
	custom := acmeTenantFixture()
	custom.Status.Domain = "acme.example"
	user := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.SingleUserTenantName}}

	for _, c := range []struct {
		what      string
		mode      string
		tenant    *gentianov1alpha1.Tenant
		component string
		profile   *gentianov1alpha1.ComponentProfile
		entry     int
		host      string
		want      string
	}{
		{"a tenant's app, tenancy multi", gentianov1alpha1.TenancyModeMulti, acmeTenantFixture(), "files", office, 0,
			"cloud.acme.k.example", "frame-ancestors 'self' https://desktop.acme.k.example https://editor.acme.k.example"},
		{"the program it embeds", gentianov1alpha1.TenancyModeMulti, acmeTenantFixture(), "files", office, 1,
			"editor.acme.k.example", "frame-ancestors 'self' https://desktop.acme.k.example https://cloud.acme.k.example"},
		{"a tenant's desktop", gentianov1alpha1.TenancyModeMulti, acmeTenantFixture(), "desktop", desktop, 0,
			"desktop.acme.k.example", "frame-ancestors 'self'"},
		{"the user tenant's app, tenancy single", gentianov1alpha1.TenancyModeSingle, user, "files", office, 0,
			"cloud.k.example", "frame-ancestors 'self' https://desktop.k.example https://editor.k.example"},
		{"the user tenant's desktop", gentianov1alpha1.TenancyModeSingle, user, "desktop", desktop, 0,
			"desktop.k.example", "frame-ancestors 'self'"},
		{"an app on a tenant's own domain", gentianov1alpha1.TenancyModeMulti, custom, "files", office, 0,
			"cloud.acme.example", "frame-ancestors 'self' https://desktop.acme.example https://editor.acme.example"},
		{"the platform's admin console", gentianov1alpha1.TenancyModeMulti, platformTenantFixture(), "admin-console", admin, 0,
			"admin.platform.k.example", "frame-ancestors 'self' https://platform.k.example"},
		{"the platform's desktop", gentianov1alpha1.TenancyModeSingle, platformTenantFixture(), "desktop", desktop, 0,
			"platform.k.example", "frame-ancestors 'self'"},
	} {
		r := &ComponentReconciler{KernelDomain: kd, KernelRealm: "kernel", TenancyMode: c.mode}
		zone := r.zoneOf(c.tenant)
		comp := &gentianov1alpha1.Component{}
		comp.Name, comp.Namespace = c.component, "tenant-"+c.tenant.Name
		e := &c.profile.Spec.Expose[c.entry]
		host := exposureHost(zone, comp, e)
		if host != c.host {
			t.Errorf("%s: host %s, want %s", c.what, host, c.host)
			continue
		}
		route := buildExposureRoute(comp, c.component+"-"+e.Name, host, zone, e,
			exposureAuthz(c.tenant, comp, c.profile, false), kd, componentFramers(zone, comp, c.profile, host))
		for _, rule := range route.Spec.Rules {
			if len(rule.Filters) != 1 || rule.Filters[0].ResponseHeaderModifier == nil {
				t.Fatalf("%s: rule %v carries no frame policy", c.what, rule.Matches)
			}
			m := rule.Filters[0].ResponseHeaderModifier
			if len(m.Remove) != 1 || m.Remove[0] != "X-Frame-Options" || len(m.Set) != 1 || m.Set[0].Name != "Content-Security-Policy" {
				t.Fatalf("%s: %+v", c.what, m)
			}
			got := m.Set[0].Value
			if got != c.want {
				t.Errorf("%s:\n  got  %s\n  want %s", c.what, got, c.want)
			}
			if strings.Contains(got, "*") {
				t.Errorf("%s: a wildcard names hosts nobody listed: %s", c.what, got)
			}
			if !zone.kernel && strings.Contains(got, "https://"+platformDesktopHost(kd)) {
				t.Errorf("%s: the platform's desktop may frame a tenant's component: %s", c.what, got)
			}
		}
	}
}

// Deleting the user tenant of a single-tenancy cluster must not take the
// kernel's own names with it: its domain is the cluster's.
func TestDirectlyUnder(t *testing.T) {
	for host, want := range map[string]bool{
		"k.example": true, "desktop.k.example": true, "platform.k.example": true,
		"admin.platform.k.example": false, "desktop.acme.k.example": false,
		"desktop.acme.example": false, "xk.example": false, ".k.example": false,
	} {
		if got := directlyUnder(host, "k.example"); got != want {
			t.Errorf("directlyUnder(%q) = %v, want %v", host, got, want)
		}
	}
	if directlyUnder("anything", "") {
		t.Error("an empty domain has hosts under it")
	}
}

// The reconciler's half of the mode: a tenant a single-tenancy cluster may
// not carry -- there because the mode was changed from multi under it -- is
// not provisioned any further, says why on its status, and loses nothing.
// The platform tenant and the user tenant go on.
func TestTheReconcilerHoldsATenantTheModeRefusesAndRemovesNothing(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	acme := acmeTenantFixture()
	ns := &corev1.Namespace{}
	ns.Name = tenantNamespaceName(acme)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(acme, ns, platformTenantFixture(), singleUserTenantFixture()).
		WithStatusSubresource(&gentianov1alpha1.Tenant{}).Build()
	r := &TenantReconciler{Client: c, Scheme: s, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "single"}

	state := &tenantReconcileState{tenant: acme}
	if _, err := r.reconcileTenantStagePreflight(ctx, state); err != nil {
		t.Fatal(err)
	}
	if !state.blocked {
		t.Fatal("a second user tenant was reconciled on under single")
	}
	got := &gentianov1alpha1.Tenant{}
	if err := c.Get(ctx, types.NamespacedName{Name: "acme"}, got); err != nil {
		t.Fatalf("the refused tenant is gone: %v", err)
	}
	var said string
	for _, cond := range got.Status.Conditions {
		if cond.Reason == "TenancyConstraint" && cond.Status == metav1.ConditionFalse {
			said = cond.Message
		}
	}
	if !strings.Contains(said, "tenancy mode is single") || !strings.Contains(said, `"acme"`) {
		t.Fatalf("the tenant's status does not say why: %q (conditions %+v)", said, got.Status.Conditions)
	}
	if got.Status.Phase != gentianov1alpha1.TenantPhaseDegraded {
		t.Fatalf("phase = %q", got.Status.Phase)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: ns.Name}, &corev1.Namespace{}); err != nil {
		t.Fatalf("the refused tenant's namespace was removed: %v", err)
	}
	for _, tenant := range []*gentianov1alpha1.Tenant{platformTenantFixture(), singleUserTenantFixture()} {
		state := &tenantReconcileState{tenant: tenant}
		if _, err := r.reconcileTenantStagePreflight(ctx, state); err != nil || state.blocked {
			t.Fatalf("%s was held under single: blocked=%v err=%v", tenant.Name, state.blocked, err)
		}
	}
	// Under multi nothing is held.
	multi := &TenantReconciler{Client: c, Scheme: s, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	state = &tenantReconcileState{tenant: acmeTenantFixture()}
	if _, err := multi.reconcileTenantStagePreflight(ctx, state); err != nil || state.blocked {
		t.Fatalf("multi held a tenant: blocked=%v err=%v", state.blocked, err)
	}
}
