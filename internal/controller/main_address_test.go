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
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

const mainKD = "k.example"

// websiteProfile is the fixture profile of a static website, under another
// name when a test needs a second one.
func websiteProfile(t *testing.T, name string) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	raw, err := os.ReadFile("testdata/main-address/website-profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := &gentianov1alpha1.ComponentProfile{}
	if err := yaml.UnmarshalStrict(raw, p); err != nil {
		t.Fatalf("the fixture profile does not parse: %v", err)
	}
	p.Name = name
	return p
}

func conciergeProfile() *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = "concierge"
	p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name: "front", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone,
		Apex: true, Paths: []string{"/"},
		Backend: gentianov1alpha1.BackendRef{Service: "concierge", Port: 8080},
	}}
	return p
}

func readyTenant(tenant *gentianov1alpha1.Tenant) *gentianov1alpha1.Tenant {
	tenant.Status.Phase = gentianov1alpha1.TenantPhaseReady
	return tenant
}

func platformWithConcierge() *gentianov1alpha1.Tenant {
	p := readyTenant(platformTenantFixture())
	p.Spec.Exposures = []gentianov1alpha1.TenantExposure{{Install: "concierge", ExposureName: "front", Owner: "installer"}}
	return p
}

// approved is the approver's entry for a website on the main address.
func approved(install string, published time.Time) gentianov1alpha1.TenantExposure {
	at := metav1.NewTime(published)
	return gentianov1alpha1.TenantExposure{
		Install: install, ExposureName: "site", Owner: "u-uma", Apex: true, PublishedAt: &at,
		ReviewAt: metav1.NewTime(published.Add(365 * 24 * time.Hour)),
	}
}

func mainInputs(t *testing.T, mode string, tenants ...*gentianov1alpha1.Tenant) mainAddressInputs {
	t.Helper()
	in := mainAddressInputs{
		Profiles: map[string]*gentianov1alpha1.ComponentProfile{
			"concierge": conciergeProfile(),
			"website":   websiteProfile(t, "website"),
			"blog":      websiteProfile(t, "blog"),
		},
		KernelDomain: mainKD, KernelRealm: "kernel", TenancyMode: mode, Now: time.Now(),
	}
	for _, tenant := range tenants {
		in.Tenants = append(in.Tenants, *tenant)
	}
	return in
}

// Who may hold the cluster's main address: the user tenant of a
// single-tenancy cluster, with the profile's word and the approver's, one
// surface at a time. Everybody else is told why not.
func TestWhoHoldsTheMainAddress(t *testing.T) {
	now := time.Now()
	withSite := func(tenant *gentianov1alpha1.Tenant, e ...gentianov1alpha1.TenantExposure) *gentianov1alpha1.Tenant {
		tenant.Spec.Exposures = append(tenant.Spec.Exposures, e...)
		return tenant
	}
	unasked := approved("website", now)
	unasked.Apex = false
	expired := approved("website", now.Add(-48*time.Hour))
	expired.ExpiresAt = exposureEnds(now.Add(-time.Hour))
	custom := withSite(readyTenant(singleUserTenantFixture()), approved("website", now))
	custom.Status.Domain = "acme.example"

	cases := []struct {
		what   string
		mode   string
		tenant *gentianov1alpha1.Tenant
		holds  bool
		reason string
	}{
		{"single, approved for the main address", "single", withSite(readyTenant(singleUserTenantFixture()), approved("website", now)), true, "Published"},
		{"multi: the main address is the sign-in form", "multi", withSite(readyTenant(singleUserTenantFixture()), approved("website", now)), false, "MultiTenancy"},
		{"multi, another tenant", "multi", withSite(readyTenant(acmeTenantFixture()), approved("website", now)), false, "MultiTenancy"},
		{"single, a tenant that is not the user tenant", "single", withSite(readyTenant(acmeTenantFixture()), approved("website", now)), false, "NotTheUserTenant"},
		{"the platform tenant gets nothing beyond its own page", "single", withSite(platformWithConcierge(), approved("website", now)), false, "NotTheUserTenant"},
		{"published, but not for the main address", "single", withSite(readyTenant(singleUserTenantFixture()), unasked), false, "NotRequested"},
		{"the approval expired", "single", withSite(readyTenant(singleUserTenantFixture()), expired), false, "Expired"},
		{"the tenant is not Ready yet", "single", withSite(singleUserTenantFixture(), approved("website", now)), false, "TenantNotReady"},
		{"the tenant is on a domain of its own", "single", custom, false, "OwnDomain"},
	}
	for _, tc := range cases {
		in := mainInputs(t, tc.mode, platformWithConcierge(), tc.tenant)
		if tc.tenant.Name == "platform" {
			in = mainInputs(t, tc.mode, tc.tenant)
		}
		holder := mainAddressHolder(in)
		if (holder != nil) != tc.holds {
			t.Errorf("%s: holder = %v, want held=%v", tc.what, holder, tc.holds)
		}
		verdict, applies := mainAddressVerdictFor(in, tc.tenant, "website")
		if !applies || verdict.Reason != tc.reason || (verdict.Entry != "") != tc.holds {
			t.Errorf("%s: verdict = %+v (applies=%v), want reason %s", tc.what, verdict, applies, tc.reason)
		}
		if !tc.holds && !strings.Contains(verdict.Message, "othing is published") && tc.reason != "TenantNotReady" {
			t.Errorf("%s: the message does not say that nothing is published: %q", tc.what, verdict.Message)
		}
	}

	// An approval for the main address of an entry the profile did not
	// declare for it publishes nothing either: both have to say it.
	plain := websiteProfile(t, "website")
	plain.Spec.Expose[0].Apex = false
	plain.Spec.Expose[0].SubDomain = "site"
	in := mainInputs(t, "single", platformWithConcierge(), withSite(readyTenant(singleUserTenantFixture()), approved("website", now)))
	in.Profiles["website"] = plain
	if holder := mainAddressHolder(in); holder != nil {
		t.Fatalf("an entry the profile does not declare for the main address holds it: %v", holder)
	}
	if v, _ := mainAddressVerdictFor(in, &in.Tenants[1], "website"); v.Reason != "NotAMainAddressEntry" {
		t.Fatalf("verdict = %+v", v)
	}

	// A website may not declare the platform's paths.
	for _, path := range []string{"/branding/", "/branding/logo/", "/sign-in", "/sign-in/help/", "/.well-known/acme-challenge/"} {
		grabby := websiteProfile(t, "website")
		grabby.Spec.Expose[0].Paths = []string{"/", path}
		in := mainInputs(t, "single", platformWithConcierge(), withSite(readyTenant(singleUserTenantFixture()), approved("website", now)))
		in.Profiles["website"] = grabby
		if holder := mainAddressHolder(in); holder != nil {
			t.Errorf("a website declaring %s holds the main address", path)
		}
		if v, _ := mainAddressVerdictFor(in, &in.Tenants[1], "website"); v.Reason != "ReservedPath" {
			t.Errorf("%s: verdict = %+v", path, v)
		}
	}
	// The rest of /.well-known/ is the website's to declare.
	matrix := websiteProfile(t, "website")
	matrix.Spec.Expose[0].Paths = []string{"/", "/.well-known/matrix/"}
	in = mainInputs(t, "single", platformWithConcierge(), withSite(readyTenant(singleUserTenantFixture()), approved("website", now)))
	in.Profiles["website"] = matrix
	if mainAddressHolder(in) == nil {
		t.Fatalf("a website declaring /.well-known/matrix/ is refused")
	}
}

// One surface at a time: the one published first keeps the address, and the
// second is told who holds it.
func TestASecondWebsiteIsRefusedTheMainAddress(t *testing.T) {
	now := time.Now()
	user := readyTenant(singleUserTenantFixture())
	// Listed second-first: the order in the Tenant is not what decides.
	user.Spec.Exposures = []gentianov1alpha1.TenantExposure{
		approved("blog", now), approved("website", now.Add(-time.Hour)),
	}
	in := mainInputs(t, "single", platformWithConcierge(), user)
	holder := mainAddressHolder(in)
	if holder == nil || holder.Install != "website" {
		t.Fatalf("holder = %v, want the one published first", holder)
	}
	second, applies := mainAddressVerdictFor(in, user, "blog")
	if !applies || second.Entry != "" || second.Reason != "HeldByAnother" || !strings.Contains(second.Message, "website/site") {
		t.Fatalf("the second website's verdict = %+v; it must be refused and name the holder", second)
	}
	// Withdrawing the first hands the address on.
	user.Spec.Exposures = user.Spec.Exposures[:1]
	if holder := mainAddressHolder(mainInputs(t, "single", platformWithConcierge(), user)); holder == nil || holder.Install != "blog" {
		t.Fatalf("after the withdrawal, holder = %v", holder)
	}
}

// What the kernel's front door does with the bare domain and www, in every
// case. Only a website that is serving changes anything.
func TestTheFrontDoorWithAWebsiteOnTheMainAddress(t *testing.T) {
	platform := *platformWithConcierge()
	user := *readyTenant(singleUserTenantFixture())

	specs := func(mode string, website bool) []kernelHTTPRouteSpec {
		tenants := []gentianov1alpha1.Tenant{platform, user}
		domains, names := zonedTenantDomains(tenants, mainKD, mode)
		door := kernelFrontDoorOf(tenants, mainKD, "kernel", mode)
		door.website = website && door.userDesktop != ""
		return kernelHTTPRouteSpecs(mainKD, domains, nil, names, false, "c1", true, true, door)
	}
	bareDomain := func(all []kernelHTTPRouteSpec) map[string]string {
		out := map[string]string{}
		for _, s := range all {
			if s.host != mainKD {
				continue
			}
			var matched []string
			for _, m := range s.rules[0].Matches {
				matched = append(matched, string(*m.Path.Type)+" "+*m.Path.Value)
			}
			out[s.name] = strings.Join(matched, ", ") + " -> " + string(*s.rules[0].Filters[0].RequestRedirect.Hostname)
		}
		return out
	}

	// multi: untouched, whatever anybody published.
	for _, website := range []bool{false, true} {
		all := specs("multi", website)
		if got := redirectsOf(all); got["www."+mainKD] != mainKD || got["console."+mainKD] != mainKD {
			t.Fatalf("multi: www -> %q, console -> %q", got["www."+mainKD], got["console."+mainKD])
		}
		if got := bareDomain(all); len(got) != 0 {
			t.Fatalf("multi: the kernel routes the bare domain: %v", got)
		}
	}

	// single, no website -- which is also a website approved but not up yet,
	// and the address after a withdrawal: the front page and /sign-in lead to
	// the desktop, and so does www.
	all := specs("single", false)
	if got := redirectsOf(all)["www."+mainKD]; got != "console."+mainKD {
		t.Fatalf("single, no website: www -> %q", got)
	}
	want := "Exact /, PathPrefix /sign-in -> console." + mainKD
	if got := bareDomain(all); got[kernelRouteApexPerimeterRedirect] != want || got[kernelRouteApexRedirect] != want {
		t.Fatalf("single, no website: the bare domain = %v", got)
	}

	// single, a website serving: the front page is no longer sent on, and
	// /sign-in still is. www leads to the bare domain, path kept.
	all = specs("single", true)
	if got := redirectsOf(all)["www."+mainKD]; got != mainKD {
		t.Fatalf("single, website: www -> %q, want the bare domain", got)
	}
	for _, s := range all {
		if s.name == kernelRouteWWWRedirect && s.rules[0].Filters[0].RequestRedirect.Path != nil {
			t.Fatalf("www rewrites the path; a page of the website must stay that page")
		}
	}
	want = "PathPrefix /sign-in -> console." + mainKD
	if got := bareDomain(all); got[kernelRouteApexPerimeterRedirect] != want || got[kernelRouteApexRedirect] != want {
		t.Fatalf("single, website: the bare domain = %v; /sign-in must still lead to the desktop and / must not", got)
	}
	// The desktop is still where it was.
	if to, routed := redirectsOf(all)["console."+mainKD]; routed {
		t.Fatalf("single, website: console.<kernel> is redirected to %q", to)
	}
}

// mainRoute is one rule on the bare domain's listener, for winnerOn.
type mainRoute struct {
	owner string
	exact bool
	path  string
}

// winnerOn resolves a request path the way the Gateway does: an exact match
// first, then the longest prefix by whole segments. A tie between two owners
// is decided by the routes' ages, which nobody controls, so it fails.
func winnerOn(t *testing.T, routes []mainRoute, path string) string {
	t.Helper()
	best, bestRank, tied := "", -1, false
	for _, r := range routes {
		rank := -1
		switch {
		case r.exact && r.path == path:
			rank = 1 << 20
		case !r.exact && pathWithin(path, r.path):
			rank = len(strings.TrimSuffix(r.path, "/"))
		}
		if rank < 0 {
			continue
		}
		switch {
		case rank > bestRank:
			best, bestRank, tied = r.owner, rank, false
		case rank == bestRank && r.owner != best:
			tied = true
		}
	}
	if tied {
		t.Fatalf("%s: two routes of different owners match equally; their age would decide", path)
	}
	return best
}

func rulesOf(owner string, rules []gatewayv1.HTTPRouteRule) []mainRoute {
	var out []mainRoute
	for _, rule := range rules {
		for _, m := range rule.Matches {
			out = append(out, mainRoute{owner: owner, exact: *m.Path.Type == gatewayv1.PathMatchExact, path: *m.Path.Value})
		}
	}
	return out
}

// mainAddressCluster is a single- or multi-tenancy cluster with the
// concierge published and, when asked, a website approved for the main
// address. reconcile brings both components' perimeters to that state.
type mainAddressCluster struct {
	t         *testing.T
	c         client.Client
	r         *ComponentReconciler
	gw        *GatewayPlatformReconciler
	platform  *gentianov1alpha1.Tenant
	user      *gentianov1alpha1.Tenant
	concierge *gentianov1alpha1.Component
	website   *gentianov1alpha1.Component
}

func newMainAddressCluster(t *testing.T, mode string) *mainAddressCluster {
	t.Helper()
	scheme := perimeterScheme()
	m := &mainAddressCluster{t: t, platform: platformWithConcierge(), user: readyTenant(singleUserTenantFixture())}
	m.concierge = componentFor("concierge", tenantNamespaceName(m.platform))
	m.website = componentFor("website", tenantNamespaceName(m.user))
	m.c = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(m.platform, m.user, conciergeProfile(), websiteProfile(t, "website"), m.concierge, m.website).Build()
	m.r = &ComponentReconciler{Client: m.c, Scheme: scheme, KernelDomain: mainKD, KernelRealm: "kernel", TenancyMode: mode}
	m.gw = &GatewayPlatformReconciler{Client: m.c, KernelDomain: mainKD, KernelRealm: "kernel", TenancyMode: mode}
	return m
}

// publish records the approver's entry on the Tenant, as Argo would apply
// the director's commit, and hands it to the Component.
func (m *mainAddressCluster) publish(exposures ...gentianov1alpha1.TenantExposure) {
	m.t.Helper()
	m.user.Spec.Exposures = exposures
	if err := m.c.Update(context.Background(), m.user); err != nil {
		m.t.Fatal(err)
	}
	m.website.Spec.Exposures = tenantExposures(m.user, "website")
}

func (m *mainAddressCluster) reconcile() {
	m.t.Helper()
	ctx := context.Background()
	m.concierge.Spec.Exposures = tenantExposures(m.platform, "concierge")
	if _, err := m.r.ensurePerimeter(ctx, m.concierge, conciergeProfile(), m.platform, m.r.zoneOf(m.platform)); err != nil {
		m.t.Fatal(err)
	}
	if _, err := m.r.ensurePerimeter(ctx, m.website, websiteProfile(m.t, "website"), m.user, m.r.zoneOf(m.user)); err != nil {
		m.t.Fatal(err)
	}
}

// proxyUp marks the website's publishing proxy as having a pod that answers.
func (m *mainAddressCluster) proxyUp() {
	m.t.Helper()
	ctx := context.Background()
	d := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: layout.TenantDMZ("user"), Name: perimeterName(m.website, "site")}
	if err := m.c.Get(ctx, key, d); err != nil {
		m.t.Fatalf("the website has no publishing proxy: %v", err)
	}
	d.Status.AvailableReplicas = 1
	if err := m.c.Status().Update(ctx, d); err != nil {
		m.t.Fatal(err)
	}
}

// routes are every rule that can answer on the bare domain: the kernel's,
// the concierge's and the website's, as the cluster holds them now.
func (m *mainAddressCluster) routes() []mainRoute {
	m.t.Helper()
	ctx := context.Background()
	tenants := &gentianov1alpha1.TenantList{}
	if err := m.c.List(ctx, tenants); err != nil {
		m.t.Fatal(err)
	}
	door := kernelFrontDoorOf(tenants.Items, mainKD, "kernel", m.gw.TenancyMode)
	if door.userDesktop != "" {
		serving, err := m.gw.mainAddressWebsiteServing(ctx, tenants.Items)
		if err != nil {
			m.t.Fatal(err)
		}
		door.website = serving
	}
	domains, names := zonedTenantDomains(tenants.Items, mainKD, m.gw.TenancyMode)
	var out []mainRoute
	for _, s := range kernelHTTPRouteSpecs(mainKD, domains, nil, names, false, "c1", true, true, door) {
		if s.host == mainKD && s.gateway == PerimeterGatewayName {
			out = append(out, rulesOf("desktop", s.rules)...)
		}
	}
	list := &gatewayv1.HTTPRouteList{}
	if err := m.c.List(ctx, list); err != nil {
		m.t.Fatal(err)
	}
	for i := range list.Items {
		route := &list.Items[i]
		if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != mainKD {
			continue
		}
		if got := string(*route.Spec.ParentRefs[0].SectionName); got != perimeterListenerName(mainKD) {
			m.t.Fatalf("route %s/%s is on listener %s, not the bare domain's", route.Namespace, route.Name, got)
		}
		out = append(out, rulesOf(route.Labels["gentianos.io/component"], route.Spec.Rules)...)
	}
	return out
}

func (m *mainAddressCluster) expect(what string, want map[string]string) {
	m.t.Helper()
	routes := m.routes()
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if got := winnerOn(m.t, routes, p); got != want[p] {
			m.t.Errorf("%s: %s is answered by %q, want %q", what, p, got, want[p])
		}
	}
}

// The main address, path by path, in every state it can be in. The
// platform's paths are the platform's in all of them.
func TestTheMainAddressPathByPath(t *testing.T) {
	now := time.Now()

	// multi: the concierge's, whole. A website approved for the main address
	// is not published.
	multi := newMainAddressCluster(t, "multi")
	multi.publish(approved("website", now))
	multi.reconcile()
	multi.expect("multi", map[string]string{
		"/": "concierge", "/sign-in": "concierge", "/sign-in/main.js": "concierge",
		"/branding/brand.css": "concierge", "/about": "concierge",
		"/.well-known/acme-challenge/x": "concierge",
	})
	if cond := apimeta.FindStatusCondition(multi.website.Status.Conditions, ConditionMainAddress); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "MultiTenancy" {
		t.Fatalf("multi: the website's condition = %+v", cond)
	}

	withoutWebsite := map[string]string{
		"/": "desktop", "/sign-in": "desktop", "/sign-in/main.js": "desktop",
		"/branding/brand.css": "concierge", "/branding/brand.json": "concierge",
		"/about": "concierge", "/.well-known/acme-challenge/x": "concierge",
	}

	// single, no website.
	single := newMainAddressCluster(t, "single")
	single.reconcile()
	single.expect("single, no website", withoutWebsite)
	if cond := apimeta.FindStatusCondition(single.website.Status.Conditions, ConditionMainAddress); cond != nil {
		t.Fatalf("a component nobody published for the main address reports %+v", cond)
	}

	// single, published without the approver saying it is for the main
	// address: nothing changes.
	unasked := approved("website", now)
	unasked.Apex = false
	single.publish(unasked)
	single.reconcile()
	single.expect("single, not asked for the main address", withoutWebsite)
	if cond := apimeta.FindStatusCondition(single.website.Status.Conditions, ConditionMainAddress); cond == nil || cond.Reason != "NotRequested" {
		t.Fatalf("condition = %+v", cond)
	}

	// single, approved, and the proxy has no pod yet: the front page still
	// leads to the desktop. The platform's paths are the platform's.
	single.publish(approved("website", now))
	single.reconcile()
	single.expect("single, website not up yet", map[string]string{
		"/": "desktop", "/sign-in": "desktop", "/sign-in/main.js": "desktop",
		"/branding/brand.css": "concierge", "/.well-known/acme-challenge/x": "concierge",
		"/.well-known/pki-validation/x": "concierge",
	})
	cond := apimeta.FindStatusCondition(single.website.Status.Conditions, ConditionMainAddress)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Published" || !strings.Contains(cond.Message, "https://"+mainKD+"/sign-in") {
		t.Fatalf("condition = %+v", cond)
	}

	// single, the website serving.
	single.proxyUp()
	single.expect("single, website serving", map[string]string{
		"/": "website", "/about": "website", "/blog/2026/hello": "website",
		"/sign-in": "desktop", "/sign-in/": "desktop", "/sign-in/main.js": "desktop",
		"/branding/brand.css": "concierge", "/branding/brand.json": "concierge",
		"/.well-known/acme-challenge/x": "concierge", "/.well-known/pki-validation/x": "concierge",
		// The rest of /.well-known/ is the website's: a Matrix delegation,
		// a security.txt.
		"/.well-known/matrix/server": "website", "/.well-known/security.txt": "website",
		// Not the platform's path, only one that begins like it.
		"/branding-guide": "website", "/sign-inn": "website",
	})

	// The website's route: from the tenant's DMZ, on the bare domain's
	// listener of the perimeter Gateway, to its own proxy and nothing else.
	route := &gatewayv1.HTTPRoute{}
	key := types.NamespacedName{Namespace: layout.TenantDMZ("user"), Name: perimeterName(single.website, "site")}
	if err := single.c.Get(context.Background(), key, route); err != nil {
		t.Fatalf("no route for the website: %v", err)
	}
	if string(route.Spec.ParentRefs[0].Name) != PerimeterGatewayName {
		t.Fatalf("the website is on the %s gateway; it must carry no session", route.Spec.ParentRefs[0].Name)
	}
	backend := route.Spec.Rules[0].BackendRefs[0]
	if string(backend.Name) != key.Name || backend.Namespace != nil {
		t.Fatalf("the website's route leads to %+v, not its own proxy in the DMZ", backend.BackendObjectReference)
	}

	// Withdrawn: the address is what it was, and nothing of the website is
	// left in the DMZ.
	single.publish()
	single.reconcile()
	single.expect("single, after the withdrawal", withoutWebsite)
	if err := single.c.Get(context.Background(), key, route); err == nil {
		t.Fatalf("the website's route outlived its withdrawal")
	}
	if err := single.c.Get(context.Background(), key, &appsv1.Deployment{}); err == nil {
		t.Fatalf("the website's proxy outlived its withdrawal")
	}
	if cond := apimeta.FindStatusCondition(single.website.Status.Conditions, ConditionMainAddress); cond != nil {
		t.Fatalf("after the withdrawal the component still reports %+v", cond)
	}
}

// The website's proxy: the platform's paths are refused there too, no
// cookie goes in or comes out, and every answer says nosniff.
func TestTheWebsitesProxyOnTheMainAddress(t *testing.T) {
	entry := &websiteProfile(t, "website").Spec.Expose[0]
	cfg := perimeterProxyConfig(entry, "website.tenant-user.svc.cluster.local", 8080, true)
	for _, want := range []string{
		"location /branding/ { return 404; }",
		"location /sign-in { return 404; }",
		"location /.well-known/acme-challenge/ { return 404; }",
		"location /.well-known/pki-validation/ { return 404; }",
		"proxy_hide_header Set-Cookie;",
		`proxy_set_header Cookie "";`,
		`proxy_set_header Authorization "";`,
		`proxy_set_header X-Gentian-Subject "";`,
		"add_header X-Content-Type-Options nosniff always;",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the website's proxy configuration lacks %q", want)
		}
	}
	// Any other surface is as it was: this is the main address's alone.
	other := perimeterProxyConfig(entry, "website.tenant-user.svc.cluster.local", 8080, false)
	if strings.Contains(other, "/branding/") || strings.Contains(other, "nosniff") {
		t.Errorf("a surface that is not on the main address got its rules:\n%s", other)
	}
}

// The bare domain has a listener for the website even on a cluster that
// publishes no page of its own there, with the cluster's certificate; and a
// website that does not hold the address adds none.
func TestTheWebsiteHasAListenerOnTheBareDomain(t *testing.T) {
	user := readyTenant(singleUserTenantFixture())
	user.Spec.Exposures = []gentianov1alpha1.TenantExposure{approved("website", time.Now())}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"website": websiteProfile(t, "website")}
	bare := func(mode string) *gatewayv1.Listener {
		gw := buildPerimeterGateway(mainKD, mode, "kernel", []gentianov1alpha1.Tenant{*readyTenant(platformTenantFixture()), *user}, profiles)
		for i := range gw.Spec.Listeners {
			if h := gw.Spec.Listeners[i].Hostname; h != nil && string(*h) == mainKD {
				return &gw.Spec.Listeners[i]
			}
		}
		return nil
	}
	l := bare("single")
	if l == nil {
		t.Fatalf("the website has no listener on the bare domain")
	}
	if ref := l.TLS.CertificateRefs[0]; string(ref.Name) != kernelWildcardTLSSecretName || ref.Namespace != nil {
		t.Fatalf("the bare domain's certificate = %+v, want the cluster's own", ref)
	}
	if bare("multi") != nil {
		t.Fatalf("multi: a tenant's website got a listener on the bare domain")
	}
}
