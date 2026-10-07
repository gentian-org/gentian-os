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
	desktopEntry      = &gentianov1alpha1.ExposureSpec{Name: "web", SubDomain: "console"}
	adminEntry        = &gentianov1alpha1.ExposureSpec{Name: "web", SubDomain: "admin"}
	appEntry          = &gentianov1alpha1.ExposureSpec{Name: "web", SubDomain: "cloud"}
	appNamelessEntry  = &gentianov1alpha1.ExposureSpec{Name: "web"}
	conciergeApexSpec = &gentianov1alpha1.ExposureSpec{Name: "front", Apex: true, Surface: gentianov1alpha1.SurfacePerimeter}
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
		{"user desktop, single", "single", singleUserTenantFixture(), "desktop", desktopEntry, "console.k.example"},
		{"user admin console, single", "single", singleUserTenantFixture(), "admin-console", adminEntry, "admin.k.example"},
		{"user app with a label, single", "single", singleUserTenantFixture(), "nextcloud", appEntry, "cloud.k.example"},
		{"user app by its name, single", "single", singleUserTenantFixture(), "wiki", appNamelessEntry, "wiki.k.example"},
		{"user tenant asks for the bare domain, single", "single", singleUserTenantFixture(), "x", conciergeApexSpec, ""},
		// Under multi, a tenant named user is an ordinary tenant.
		{"tenant named user, multi", "multi", singleUserTenantFixture(), "desktop", desktopEntry, "console.user.k.example"},
		{"a tenant, multi: desktop", "multi", acmeTenantFixture(), "desktop", desktopEntry, "console.acme.k.example"},
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
		route := buildExposureRoute(comp, component+"-web", host, zone, &entry, routeAuthz{relation: "can_enter", object: "tenant:" + tenant.Name}, kd)
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
		{"single", singleUserTenantFixture(), "desktop", desktopEntry, "console.k.example", wildcardListenerName},
		{"single", singleUserTenantFixture(), "admin-console", adminEntry, "admin.k.example", wildcardListenerName},
		{"multi", acmeTenantFixture(), "desktop", desktopEntry, "console.acme.k.example", tenantGatewayListenerName("acme")},
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

// The bare domain, www and console.<kernel>, per mode.
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

	// multi: console.<kernel> and www lead to the bare domain, which the
	// kernel does not route -- it is the concierge's, on the perimeter.
	// A tenant named user changes nothing.
	for _, tenants := range [][]gentianov1alpha1.Tenant{{platform}, {platform, acme}, {platform, user}, {platform, user, acme}} {
		got := redirectsOf(specsFor("multi", tenants...))
		if got["www."+kd] != kd || got["console."+kd] != kd {
			t.Fatalf("multi, %d tenants: www -> %q, console -> %q, want both the bare domain", len(tenants), got["www."+kd], got["console."+kd])
		}
		if to, routed := got[kd]; routed {
			t.Fatalf("multi, %d tenants: the kernel routes the bare domain to %q; it is the concierge's", len(tenants), to)
		}
		if to, routed := got["platform."+kd]; routed {
			t.Fatalf("multi: platform.<kernel> is redirected to %q; it is the platform's desktop", to)
		}
	}
	// A tenant's own apex still leads to that tenant's console.
	if got := redirectsOf(specsFor("multi", platform, acme)); got["acme."+kd] != "console.acme."+kd {
		t.Fatalf("multi: acme's apex -> %q", got["acme."+kd])
	}

	// single: the bare domain and www lead to the user tenant's desktop once
	// it is Ready; console.<kernel> is that desktop and is not redirected.
	got := redirectsOf(specsFor("single", platform, user))
	if got[kd] != "console."+kd || got["www."+kd] != "console."+kd {
		t.Fatalf("single: bare -> %q, www -> %q, want console.%s", got[kd], got["www."+kd], kd)
	}
	if to, routed := got["console."+kd]; routed {
		t.Fatalf("single: console.<kernel> is redirected to %q; it is the user tenant's desktop", to)
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
	if string(*rr.Hostname) != "console."+kd || rr.Path == nil || *rr.Path.ReplaceFullPath != "/" {
		t.Fatalf("single: the perimeter redirect leads to %v %v", *rr.Hostname, rr.Path)
	}
	for _, s := range specsFor("multi", platform, user) {
		if s.name == kernelRouteApexPerimeterRedirect || s.name == kernelRouteApexRedirect {
			t.Fatalf("multi: the bare domain is redirected by %s", s.name)
		}
	}
	// With a custom domain bound, the desktop is on it and the bare domain
	// follows.
	if got := redirectsOf(specsFor("single", platform, custom)); got[kd] != "console.acme.example" || got["www."+kd] != "console.acme.example" {
		t.Fatalf("single, custom domain: bare -> %q, www -> %q", got[kd], got["www."+kd])
	}
	// Before the user tenant is Ready, and before it exists: nothing claims
	// console.<kernel>, and the bare domain is sent nowhere yet.
	for _, tenants := range [][]gentianov1alpha1.Tenant{{platform}, {platform, pending}} {
		got := redirectsOf(specsFor("single", tenants...))
		if to, routed := got[kd]; routed {
			t.Fatalf("single, user tenant not Ready: the bare domain is sent to %q", to)
		}
		if to, routed := got["console."+kd]; routed {
			t.Fatalf("single, user tenant not Ready: console.<kernel> is redirected to %q", to)
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
// name that is the kernel's; console and admin are its own; and on a
// multi-tenancy cluster, or in the platform tenant, nothing is refused.
func TestAKernelsHostLabelIsRefusedOnTheClustersDomain(t *testing.T) {
	const kd = "k.example"
	profileWith := func(sub string) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{}
		p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, SubDomain: sub}}
		return p
	}
	single := zoneNamesOf(singleUserTenantFixture(), kd, "single", "kernel")
	for _, label := range append([]string{"x.platform", "a.b.id"}, kernelReservedHostLabels...) {
		comp := componentFor("thing", "tenant-user")
		refusal := reservedHostRefusal(comp, profileWith(label), single, kd)
		if refusal == "" {
			t.Errorf("%s.%s was not refused for the user tenant of a single-tenancy cluster", label, kd)
			continue
		}
		for _, want := range []string{label + "." + kd, "tenancy mode is single", "platform", "id"} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal of %q does not say %q: %s", label, want, refusal)
			}
		}
	}
	// By the component's name as well, where the entry names no label.
	if reservedHostRefusal(componentFor("llm", "tenant-user"), profileWith(""), single, kd) == "" {
		t.Error("a component named llm was not refused")
	}
	// Its own names, and any other, are fine.
	for _, label := range []string{"console", "admin", "cloud", "identity", "platformer"} {
		if refusal := reservedHostRefusal(componentFor("thing", "tenant-user"), profileWith(label), single, kd); refusal != "" {
			t.Errorf("%s was refused: %s", label, refusal)
		}
	}
	// An addon's entry is served by its base, on the base's host.
	addon := profileWith("id")
	addon.Spec.Expose[0].Backend.Component = "nextcloud"
	if refusal := reservedHostRefusal(componentFor("calendar", "tenant-user"), addon, single, kd); refusal != "" {
		t.Errorf("an entry another component serves was refused: %s", refusal)
	}
	// Not asked anywhere else: a tenant with a domain of its own cannot reach
	// the kernel's names, and the platform tenant's are below platform.
	for _, zone := range []zoneNames{
		zoneNamesOf(acmeTenantFixture(), kd, "multi", "kernel"),
		zoneNamesOf(singleUserTenantFixture(), kd, "multi", "kernel"),
		zoneNamesOf(platformTenantFixture(), kd, "single", "kernel"),
		zoneNamesOf(platformTenantFixture(), kd, "multi", "kernel"),
	} {
		if refusal := reservedHostRefusal(componentFor("thing", "tenant-x"), profileWith("id"), zone, kd); refusal != "" {
			t.Errorf("zone %+v: refused: %s", zone, refusal)
		}
	}
	// The list this refuses, pinned: a name added to the kernel's routes
	// belongs here too.
	if got := strings.Join(kernelReservedHostLabels, ","); got != "argocd,corp,headlamp,id,imap,llm,mail,mail-egress,platform,www" {
		t.Errorf("reserved labels = %s", got)
	}
}

// The desktops an app may be framed by: the platform's, at platform.<kernel>,
// and the tenant's own -- also when the tenant's domain is the cluster's.
func TestTheDesktopsAnAppMayBeFramedBy(t *testing.T) {
	if got := strings.Join(consoleOrigins("k.example", "acme.k.example"), " "); got != "https://platform.k.example https://console.acme.k.example" {
		t.Errorf("a tenant: %s", got)
	}
	if got := strings.Join(consoleOrigins("k.example", "k.example"), " "); got != "https://platform.k.example https://console.k.example" {
		t.Errorf("the user tenant of a single-tenancy cluster: %s", got)
	}
}

// Deleting the user tenant of a single-tenancy cluster must not take the
// kernel's own names with it: its domain is the cluster's.
func TestDirectlyUnder(t *testing.T) {
	for host, want := range map[string]bool{
		"k.example": true, "console.k.example": true, "platform.k.example": true,
		"admin.platform.k.example": false, "console.acme.k.example": false,
		"console.acme.example": false, "xk.example": false, ".k.example": false,
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
