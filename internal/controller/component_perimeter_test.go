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
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A Nextcloud that declares what it would publish: shared links and CalDAV.
// Declaring is not publishing — nothing exists until somebody enables it.
func nextcloudWithPerimeter() *gentianov1alpha1.ComponentProfile {
	p := nextcloudBaseProfile()
	p.Spec.Expose = append(p.Spec.Expose, gentianov1alpha1.ExposureSpec{
		Name:      "shares",
		Surface:   gentianov1alpha1.SurfacePerimeter,
		AuthMode:  gentianov1alpha1.AuthModeNone,
		SubDomain: "share",
		Paths:     []string{"/s/", "/remote.php/dav/", "/public.php"},
		DenyPaths: []string{"/remote.php/dav/systemtags/"},
		Backend:   gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080},
	})
	return p
}

func enabledShares(expiry time.Time) []gentianov1alpha1.ExposureEnablement {
	return []gentianov1alpha1.ExposureEnablement{{
		ExposureName: "shares",
		Owner:        "u-tom",
		ExpiresAt:    exposureEnds(expiry),
	}}
}

func perimeterScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	_ = gatewayv1.Install(s)
	return s
}

// The configuration is where the perimeter's security properties live, so
// they are asserted on the text the proxy actually runs.
func TestThePublishedSurfaceIsOnlyWhatWasDeclared(t *testing.T) {
	profile := nextcloudWithPerimeter()
	e := &profile.Spec.Expose[1]
	conf := perimeterProxyConfig(e, "nextcloud.tenant-acme.svc.cluster.local", 8080, false, perimeterLimitsFromEnv(edgeClientAddressHeader(context.Background(), nil)))

	// Property 1: only the declared prefixes, by whole segments, and
	// everything else refused here rather than forwarded.
	for _, want := range []string{
		`"~^/s/" 1;`, `"~^/remote\.php/dav/" 1;`, `"~^/public\.php(/|$)" 1;`,
		"if ($perimeter_published = 0) { return 404; }",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("a declared prefix, or the refusal of the rest, is missing: %s", want)
		}
	}
	// A denied path is refused even where a prefix admits it, whatever its
	// case.
	for _, want := range []string{`"~*^/remote\.php/dav/systemtags/" 1;`, "if ($perimeter_denied) { return 404; }"} {
		if !strings.Contains(conf, want) {
			t.Errorf("the denied path is not refused: %s", want)
		}
	}
	// A path with more than one reading is refused before either is asked.
	if !strings.Contains(conf, "if ($perimeter_ambiguous) { return 400; }") {
		t.Error("a path the proxy and the application could read differently must be refused")
	}

	// Properties 2 to 4: nothing of the session model goes in.
	for _, want := range []string{
		`proxy_set_header Cookie "";`,
		`proxy_set_header Authorization "";`,
		`proxy_set_header X-Forwarded-Access-Token "";`,
		`proxy_set_header X-Auth-Request-User "";`,
		`proxy_set_header X-Auth-Request-Groups "";`,
		`proxy_set_header X-Gentian-Subject "";`,
		`proxy_set_header Forwarded "";`,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("the perimeter must not carry identity inwards; missing: %s", want)
		}
	}
	// And the application's own session does not come back out, or a browser
	// would carry it to every other link on that host.
	if !strings.Contains(conf, "proxy_hide_header Set-Cookie;") {
		t.Error("a Set-Cookie from the application must not reach the public host")
	}
	if strings.Contains(conf, "proxy_pass_header Server") {
		t.Error("the answer must name the proxy, not what the application runs on")
	}
	// It reaches exactly one upstream, from exactly one place.
	if strings.Count(conf, "proxy_pass ") != 1 {
		t.Errorf("proxy_pass appears %d times, want once", strings.Count(conf, "proxy_pass "))
	}
	if !strings.Contains(conf, "proxy_pass http://nextcloud.tenant-acme.svc.cluster.local:8080;") {
		t.Error("the upstream is not the component's own Service")
	}
}

// Every header the front door sets on a route behind a session is removed
// on a route without one, by the list the front door itself keeps: a header
// added to the bouncer is stripped here without anybody remembering to.
func TestThePerimeterStripsEveryIdentityHeaderTheFrontDoorSets(t *testing.T) {
	e := &nextcloudWithPerimeter().Spec.Expose[1]
	conf := perimeterProxyConfig(e, "nextcloud.tenant-acme.svc.cluster.local", 8080, false, perimeterLimitsFromEnv(edgeClientAddressHeader(context.Background(), nil)))
	stripped := map[string]bool{}
	for _, h := range perimeterStrippedIdentityHeaders() {
		stripped[strings.ToLower(h)] = true
		if !strings.Contains(conf, fmt.Sprintf("proxy_set_header %s \"\";", h)) {
			t.Errorf("%s is in the list and not in the configuration", h)
		}
	}
	if len(bouncer.IdentityHeaders()) < 6 {
		t.Fatalf("the front door names %d identity headers; it set six when this was written", len(bouncer.IdentityHeaders()))
	}
	for _, h := range bouncer.IdentityHeaders() {
		if !stripped[strings.ToLower(h)] {
			t.Errorf("the front door sets %s and the publishing proxy lets a caller send it", h)
		}
	}
	// The sign-in route that takes no session removes the same ones.
	onACS := map[string]bool{}
	for _, h := range frontDoorIdentityHeaders {
		onACS[strings.ToLower(h)] = true
	}
	for _, h := range bouncer.IdentityHeaders() {
		if !onACS[strings.ToLower(h)] {
			t.Errorf("the front door sets %s and the sign-in route lets a caller send it", h)
		}
	}
}

// The limits are the platform's, in one place, and what the operator's
// environment may change is bounded by what the proxy starts with.
func TestThePerimeterLimits(t *testing.T) {
	for _, key := range []string{
		"PERIMETER_MAX_BODY", "PERIMETER_RATE_PER_SECOND", "PERIMETER_RATE_BURST", "PERIMETER_CONCURRENT_PER_CLIENT",
		"PERIMETER_CLIENT_ADDRESS_HEADER", "EDGE_INGRESS", "NETWORK_MODE",
	} {
		t.Setenv(key, "")
	}
	e := &nextcloudWithPerimeter().Spec.Expose[1]
	render := func() string {
		return perimeterProxyConfig(e, "nextcloud.tenant-acme.svc.cluster.local", 8080, false, perimeterLimitsFromEnv(edgeClientAddressHeader(context.Background(), nil)))
	}

	conf := render()
	for _, want := range []string{
		"client_max_body_size 10m;", "large_client_header_buffers 4 8k;", "client_header_timeout 15s;",
		"client_body_timeout 30s;", "proxy_connect_timeout 5s;", "proxy_read_timeout 60s;", "server_tokens off;",
		"underscores_in_headers off;",
		"zone=perimeter_rate:10m rate=20r/s;", "limit_req zone=perimeter_rate burst=200 nodelay;",
		"limit_conn perimeter_concurrent 100;", "limit_req_status 429;", "limit_conn_status 429;",
		`if ($request_method ~ "^(TRACE|TRACK|CONNECT)$") { return 405; }`,
		// Nothing says how the cluster is reached: the connection, which
		// nobody can forge.
		"limit_req_zone $perimeter_edge_peer ",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("the default configuration lacks %q", want)
		}
	}
	if strings.Contains(conf, "cf_connecting_ip") {
		t.Error("a header a caller can write is read on a cluster nobody said is behind the tunnel")
	}

	// Behind the tunnel, which names the caller itself.
	t.Setenv("NETWORK_MODE", "tunnel")
	conf = render()
	for _, want := range []string{"map $http_cf_connecting_ip $perimeter_client {", "limit_req_zone $perimeter_client "} {
		if !strings.Contains(conf, want) {
			t.Errorf("behind the tunnel the configuration lacks %q", want)
		}
	}

	// A cluster with an address of its own: the Gateway saw the caller, and
	// a header a caller can write is not read.
	t.Setenv("NETWORK_MODE", "static-ip")
	conf = render()
	if strings.Contains(conf, "cf_connecting_ip") || !strings.Contains(conf, "limit_req_zone $perimeter_edge_peer ") {
		t.Errorf("with an address of its own the cluster must key on the Gateway's word alone:\n%s", conf)
	}
	if !strings.Contains(conf, "proxy_set_header X-Forwarded-For $perimeter_edge_peer;") {
		t.Error("the application is not told the address the Gateway saw")
	}

	// The administrator's values, and values the proxy would not start with.
	t.Setenv("PERIMETER_MAX_BODY", "2g")
	t.Setenv("PERIMETER_RATE_PER_SECOND", "50")
	t.Setenv("PERIMETER_RATE_BURST", "500")
	t.Setenv("PERIMETER_CONCURRENT_PER_CLIENT", "10")
	t.Setenv("PERIMETER_CLIENT_ADDRESS_HEADER", "True-Client-IP")
	conf = render()
	for _, want := range []string{
		"client_max_body_size 2g;", "rate=50r/s;", "burst=500 nodelay;", "limit_conn perimeter_concurrent 10;",
		"map $http_true_client_ip $perimeter_client {",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("the administrator's value is not in force: %q", want)
		}
	}
	t.Setenv("PERIMETER_MAX_BODY", "10m; include /etc/passwd")
	t.Setenv("PERIMETER_RATE_PER_SECOND", "-1")
	t.Setenv("PERIMETER_RATE_BURST", "lots")
	t.Setenv("PERIMETER_CONCURRENT_PER_CLIENT", "0")
	t.Setenv("PERIMETER_CLIENT_ADDRESS_HEADER", "X-Bad Header;")
	conf = render()
	for _, want := range []string{"client_max_body_size 10m;", "rate=20r/s;", "burst=200 nodelay;", "limit_conn perimeter_concurrent 100;"} {
		if !strings.Contains(conf, want) {
			t.Errorf("an unusable value replaced the default: %q is missing", want)
		}
	}
	if strings.Contains(conf, "passwd") || strings.Contains(conf, "x_bad") {
		t.Errorf("an unusable value reached the configuration:\n%s", conf)
	}
	t.Setenv("PERIMETER_CLIENT_ADDRESS_HEADER", "none")
	t.Setenv("NETWORK_MODE", "tunnel")
	if conf = render(); strings.Contains(conf, "$perimeter_client") {
		t.Error("PERIMETER_CLIENT_ADDRESS_HEADER=none must read the connection even behind the tunnel")
	}
}

// An exposure that declares no paths publishes nothing. On the gateway, empty
// paths mean the whole host; here that would be the whole application.
func TestAPerimeterEntryWithNoPathsPublishesNothing(t *testing.T) {
	e := &gentianov1alpha1.ExposureSpec{
		Name: "everything", Surface: gentianov1alpha1.SurfacePerimeter,
		Backend: gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080},
	}
	conf := perimeterProxyConfig(e, "nextcloud.tenant-acme.svc.cluster.local", 8080, false, perimeterLimitsFromEnv(edgeClientAddressHeader(context.Background(), nil)))
	if strings.Contains(conf, "proxy_pass") {
		t.Fatal("an entry declaring no paths forwarded something")
	}
	if !strings.Contains(conf, "location / { return 404; }") {
		t.Fatal("it should refuse everything")
	}
}

// A path that would be configuration rather than a path is not rendered, and
// a denied path that cannot be rendered closes the entry: publishing the
// rest without it would publish what the author said not to.
func TestAPathThatWouldBeConfigurationIsNotRendered(t *testing.T) {
	e := &gentianov1alpha1.ExposureSpec{
		Name: "x", Surface: gentianov1alpha1.SurfacePerimeter,
		Paths:   []string{"/ok/", "/a b", "/x { return 200; }", "/q\"", "/semi;", "/d$uri", "/n\nl", "/dup//slash"},
		Backend: gentianov1alpha1.BackendRef{Service: "app", Port: 8080},
	}
	conf := perimeterProxyConfig(e, "app.tenant-acme.svc.cluster.local", 8080, false, perimeterLimitsFromEnv(edgeClientAddressHeader(context.Background(), nil)))
	if !strings.Contains(conf, `"~^/ok/" 1;`) {
		t.Error("the one usable path is not published")
	}
	for _, bad := range []string{"/a b", "return 200", "/semi", "$uri\"", "/dup"} {
		if strings.Contains(conf, bad) {
			t.Errorf("an unusable path reached the configuration: %q", bad)
		}
	}
	e.DenyPaths = []string{"/ok/private x"}
	conf = perimeterProxyConfig(e, "app.tenant-acme.svc.cluster.local", 8080, false, perimeterLimitsFromEnv(edgeClientAddressHeader(context.Background(), nil)))
	if strings.Contains(conf, "proxy_pass") {
		t.Error("an entry whose denied path cannot be rendered still published the rest")
	}
}

// Declaring a perimeter surface publishes nothing. Until a perimeter approver
// enables it, there is no namespace, no proxy and no route.
func TestDeclaringAPerimeterSurfacePublishesNothing(t *testing.T) {
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	comp := componentFor("nextcloud-base-ce", tenantNamespaceName(tenant))
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), nextcloudWithPerimeter(), comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}

	n, err := r.ensurePerimeter(context.Background(), comp, nextcloudWithPerimeter(), tenant, r.zoneOf(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("published %d surfaces with nothing enabled", n)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: layout.TenantDMZ(tenant.Name)}, ns); err == nil {
		t.Fatal("a DMZ namespace was created for a surface nobody enabled")
	}
}

// Enabled, it appears: a proxy, a Service, a route on the PERIMETER gateway,
// and policies that bound the DMZ in both directions.
func TestAnEnabledSurfaceIsPublishedFromTheDMZ(t *testing.T) {
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	ns := tenantNamespaceName(tenant)
	dmz := layout.TenantDMZ(tenant.Name)

	comp := componentFor("nextcloud-base-ce", ns)
	comp.Spec.Exposures = enabledShares(time.Now().Add(720 * time.Hour))
	profile := nextcloudWithPerimeter()

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), profile, comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	ctx := context.Background()

	n, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("published %d surfaces, want 1", n)
	}
	name := perimeterName(comp, "shares")

	// The DMZ is its own namespace, at its own tier.
	nsObj := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: dmz}, nsObj); err != nil {
		t.Fatalf("no DMZ namespace: %v", err)
	}
	if nsObj.Labels["gentianos.io/tier"] != string(layout.TierTenantDMZ) {
		t.Fatalf("DMZ tier = %q", nsObj.Labels["gentianos.io/tier"])
	}

	// The proxy runs there, not in the tenant's namespace.
	deploy := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: dmz}, deploy); err != nil {
		t.Fatalf("no proxy: %v", err)
	}
	pod := deploy.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("a pod on the public internet must not mount a service account token")
	}
	sc := pod.Containers[0].SecurityContext
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("proxy security context = %+v", sc)
	}

	// Published on the perimeter Gateway, which carries no session.
	route := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: dmz}, route); err != nil {
		t.Fatalf("no route: %v", err)
	}
	if got := string(route.Spec.ParentRefs[0].Name); got != PerimeterGatewayName {
		t.Fatalf("published on the %q gateway; a shared link must not sit behind a login", got)
	}
	if string(route.Spec.Hostnames[0]) != "share.acme.k.example" {
		t.Fatalf("host = %s", route.Spec.Hostnames[0])
	}

	// Out of the DMZ: one Service, and DNS.
	egress := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: name + "-egress", Namespace: dmz}, egress); err != nil {
		t.Fatalf("no egress policy: %v", err)
	}
	if len(egress.Spec.Egress) != 2 {
		t.Fatalf("the DMZ may reach %d things; want the component and DNS", len(egress.Spec.Egress))
	}
	// Into the tenant: from the DMZ only.
	// The proxy itself takes connections from the Gateway's Envoy pods and
	// from nothing else: what it believes of a caller's address is what the
	// Gateway appended, and only the Gateway may be the one saying it.
	front := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: name + "-ingress", Namespace: dmz}, front); err == nil {
		t.Fatal("with the kernel's network rules off the proxy carries their rule")
	}
	t.Setenv("KERNEL_NETWORK_POLICIES", "true")
	if _, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: name + "-ingress", Namespace: dmz}, front); err != nil {
		t.Fatalf("no ingress policy on the proxy: %v", err)
	}
	if len(front.Spec.Ingress) != 1 || len(front.Spec.Ingress[0].From) != 1 || len(front.Spec.Ingress[0].Ports) != 1 ||
		front.Spec.Ingress[0].Ports[0].Port.IntVal != perimeterProxyPort {
		t.Fatalf("the proxy admits %+v; want the Gateway on the proxy's port", front.Spec.Ingress)
	}
	if peer := front.Spec.Ingress[0].From[0]; peer.NamespaceSelector == nil || peer.PodSelector == nil ||
		peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != layout.Namespace(layout.Edge) ||
		peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "envoy" {
		t.Fatalf("the proxy admits %+v; want the Envoy pods of the edge namespace alone", peer)
	}
	if front.Spec.PodSelector.MatchLabels["gentianos.io/exposure"] == "" {
		t.Fatalf("the policy selects %v, not this exposure's proxy", front.Spec.PodSelector)
	}

	ingress := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: name + "-ingress", Namespace: ns}, ingress); err != nil {
		t.Fatalf("no ingress policy in the tenant namespace: %v", err)
	}
}

// An expiry is what makes a published link stop answering by itself. The
// worst failure this mechanism can have is a surface somebody revoked that
// still works.
func TestAnExpiredEnablementIsTakenDown(t *testing.T) {
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	ns := tenantNamespaceName(tenant)
	dmz := layout.TenantDMZ(tenant.Name)
	profile := nextcloudWithPerimeter()
	ctx := context.Background()

	comp := componentFor("nextcloud-base-ce", ns)
	comp.Spec.Exposures = enabledShares(time.Now().Add(time.Hour))
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), profile, comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	if _, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant)); err != nil {
		t.Fatal(err)
	}
	name := perimeterName(comp, "shares")
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: dmz}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("the surface was not published to begin with: %v", err)
	}

	// Time passes.
	comp.Spec.Exposures = enabledShares(time.Now().Add(-time.Minute))
	n, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("an expired enablement still published %d surfaces", n)
	}
	for _, check := range []struct {
		what string
		obj  client.Object
	}{
		{"the proxy", &appsv1.Deployment{}},
		{"its Service", &corev1.Service{}},
		{"its route", &gatewayv1.HTTPRoute{}},
		{"its configuration", &corev1.ConfigMap{}},
	} {
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: dmz}, check.obj); err == nil {
			t.Errorf("%s outlived the enablement", check.what)
		}
	}
	if err := c.Get(ctx, types.NamespacedName{Name: name + "-ingress", Namespace: ns}, &networkingv1.NetworkPolicy{}); err == nil {
		t.Error("the tenant still accepts traffic from a DMZ that publishes nothing")
	}
}

// An enablement naming an entry the profile does not declare publishes
// nothing: a surface nobody wrote down is not one a tenant can approve into
// existence.
func TestAnEnablementWithoutADeclarationPublishesNothing(t *testing.T) {
	tenant := acmeTenantFixture()
	comp := componentFor("nextcloud-base-ce", tenantNamespaceName(tenant))
	comp.Spec.Exposures = []gentianov1alpha1.ExposureEnablement{{
		ExposureName: "invented", Owner: "u-tom",
		ExpiresAt: exposureEnds(time.Now().Add(time.Hour)),
	}}
	live := livePerimeterExposures(comp, nextcloudWithPerimeter(), edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}, time.Now(), perimeterMainAddress{})
	if len(live) != 0 {
		t.Fatalf("published %d surfaces for an entry no profile declares", len(live))
	}
}

// A gateway entry is not a perimeter entry. Enabling one by its name must not
// publish the component's authenticated surface to the internet.
func TestAGatewayEntryCannotBePublishedOnThePerimeter(t *testing.T) {
	tenant := acmeTenantFixture()
	comp := componentFor("nextcloud-base-ce", tenantNamespaceName(tenant))
	comp.Spec.Exposures = []gentianov1alpha1.ExposureEnablement{{
		ExposureName: "web", Owner: "u-tom", // "web" is the gateway entry
		ExpiresAt: exposureEnds(time.Now().Add(time.Hour)),
	}}
	live := livePerimeterExposures(comp, nextcloudWithPerimeter(), edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}, time.Now(), perimeterMainAddress{})
	if len(live) != 0 {
		t.Fatalf("a gateway surface was published on the perimeter")
	}
}

// A published host needs a listener on the perimeter Gateway, or its route
// attaches to nothing. The host is the one the ROUTE is written with -- the
// entry's own label -- and not a name of the listener's own making: the two
// once differed (the entry's name against its subDomain), and a route whose
// host no listener serves answers nothing.
//
// This is the failure with no error to read: the HTTPRoute exists, the proxy
// runs, and the hostname resolves to an Envoy that has never been told to
// serve it. The route's status says NoMatchingParent in a field nobody
// watches, and the link simply does not answer.
func TestAPublishedHostGetsAListenerOnThePerimeterGateway(t *testing.T) {
	tenant := acmeTenantFixture()
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{{
		Install: "nextcloud-base-ce", ExposureName: "shares", Owner: "u-tom",
		ExpiresAt: exposureEnds(time.Now().Add(24 * time.Hour)),
	}}
	gw := buildPerimeterGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, perimeterProfiles())

	var hosts []string
	for _, l := range gw.Spec.Listeners {
		if l.Hostname != nil {
			hosts = append(hosts, string(*l.Hostname))
		}
	}
	if !containsString(hosts, "share.acme.k.example") {
		t.Fatalf("the published host has no listener; got %v", hosts)
	}
	// The identity provider's own listener is still there: publishing a
	// tenant surface must not displace the thing every login goes through.
	if !containsString(hosts, "id.k.example") {
		t.Fatalf("the identity provider lost its listener; got %v", hosts)
	}
}

// EXACT hostnames, never a wildcard. The authenticated Gateway already holds
// *.<tenant domain>, and under mergeGateways a listener is unique per port
// and hostname across the class — so a wildcard here would collide with the
// edge every other surface of that tenant is served from.
func TestThePerimeterDoesNotClaimTheTenantsWildcard(t *testing.T) {
	tenant := acmeTenantFixture()
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{{
		Install: "nextcloud-base-ce", ExposureName: "shares", Owner: "u-tom",
		ExpiresAt: exposureEnds(time.Now().Add(24 * time.Hour)),
	}}
	gw := buildPerimeterGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, perimeterProfiles())
	for _, l := range gw.Spec.Listeners {
		if l.Hostname != nil && strings.HasPrefix(string(*l.Hostname), "*") {
			t.Fatalf("the perimeter claimed a wildcard listener: %s", *l.Hostname)
		}
	}
}

// A tenant bound to a custom domain publishes under it: the entry's name
// under the domain the TenantDomain gave it, not under the kernel's.
func TestACustomDomainTenantPublishesUnderItsDomain(t *testing.T) {
	tenant := acmeTenantFixture()
	tenant.Status.Domain = "acme.example"
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{{
		Install: "nextcloud-base-ce", ExposureName: "shares", Owner: "u-tom",
		ExpiresAt: exposureEnds(time.Now().Add(24 * time.Hour)),
	}}
	gw := buildPerimeterGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, perimeterProfiles())
	var hosts []string
	for _, l := range gw.Spec.Listeners {
		if l.Hostname != nil {
			hosts = append(hosts, string(*l.Hostname))
		}
	}
	if !containsString(hosts, "share.acme.example") {
		t.Fatalf("the custom domain's host has no listener; got %v", hosts)
	}
}

// A tenant that publishes nothing adds nothing: the perimeter stays the two
// listeners the kernel needs.
func TestATenantPublishingNothingAddsNoListener(t *testing.T) {
	tenant := acmeTenantFixture()
	gw := buildPerimeterGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, perimeterProfiles())
	if len(gw.Spec.Listeners) != 2 {
		t.Fatalf("listeners = %d, want the identity provider and :80 only", len(gw.Spec.Listeners))
	}
}

// perimeterProfiles is what the Gateway is given to name a published host:
// the profiles, by the name an install carries.
func perimeterProfiles() map[string]*gentianov1alpha1.ComponentProfile {
	p := nextcloudWithPerimeter()
	return map[string]*gentianov1alpha1.ComponentProfile{p.Name: p}
}

// The platform tenant may publish on the cluster's bare domain, with the
// kernel's own certificate; no other tenant's apex entry gets a listener.
func TestOnlyThePlatformTenantPublishesOnTheBareDomain(t *testing.T) {
	front := &gentianov1alpha1.ComponentProfile{}
	front.Name = "front"
	front.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name: "site", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone,
		Apex: true, Paths: []string{"/"},
		Backend: gentianov1alpha1.BackendRef{Service: "front", Port: 8080},
	}}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{"front": front}
	published := []gentianov1alpha1.TenantExposure{{Install: "front", ExposureName: "site", Owner: "installer"}}

	platform := platformTenantFixture()
	platform.Spec.Exposures = published
	acme := acmeTenantFixture()
	acme.Spec.Exposures = published

	gw := buildPerimeterGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*platform, *acme}, profiles)
	var apex *gatewayv1.Listener
	for i, l := range gw.Spec.Listeners {
		if l.Hostname == nil {
			continue
		}
		switch string(*l.Hostname) {
		case "k.example":
			apex = &gw.Spec.Listeners[i]
		case "acme.k.example":
			t.Fatalf("a tenant other than the platform's got a listener on its bare domain")
		}
	}
	if apex == nil {
		t.Fatalf("the platform tenant's apex entry has no listener")
	}
	ref := apex.TLS.CertificateRefs[0]
	// No namespace on the reference means the Gateway's own, which is where
	// the kernel's certificate is.
	if string(ref.Name) != kernelWildcardTLSSecretName || (ref.Namespace != nil && string(*ref.Namespace) != servicesNamespace) {
		t.Fatalf("the bare domain is served with %v, want the kernel's certificate", ref)
	}
}

// An enablement that names nothing the profile declares as a perimeter entry
// gets no listener.
func TestAnEnablementOfNothingDeclaredGetsNoListener(t *testing.T) {
	tenant := acmeTenantFixture()
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{
		{Install: "nextcloud-base-ce", ExposureName: "web", Owner: "u-tom"},
		{Install: "unknown", ExposureName: "shares", Owner: "u-tom"},
	}
	gw := buildPerimeterGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, perimeterProfiles())
	if len(gw.Spec.Listeners) != 2 {
		t.Fatalf("listeners = %d, want the identity provider and :80 only", len(gw.Spec.Listeners))
	}
}

func containsString(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// exposureEnds is an expiry, for an exposure published for a while.
func exposureEnds(at time.Time) *metav1.Time {
	t := metav1.NewTime(at)
	return &t
}

// Which address a caller is counted under is asked of the cluster: the
// Gateway's Service has an address outside the cluster or it has not, and
// only without one is the tunnel's header the tunnel's word.
func TestTheClientAddressFollowsHowTheGatewayIsReached(t *testing.T) {
	for _, key := range []string{"PERIMETER_CLIENT_ADDRESS_HEADER", "EDGE_INGRESS", "NETWORK_MODE"} {
		t.Setenv(key, "")
	}
	ctx := context.Background()
	edge := func(kind corev1.ServiceType) client.Client {
		return fake.NewClientBuilder().WithScheme(perimeterScheme()).WithObjects(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "envoy-kernel-edge", Namespace: layout.Namespace(layout.Edge),
				Labels: map[string]string{"gateway.envoyproxy.io/owning-gatewayclass": GentianGatewayClassName},
			},
			Spec: corev1.ServiceSpec{Type: kind},
		}).Build()
	}
	if got := edgeClientAddressHeader(ctx, edge(corev1.ServiceTypeLoadBalancer)); got != "" {
		t.Errorf("a Gateway with an address of its own: reads %q, a header any caller can write", got)
	}
	if got := edgeClientAddressHeader(ctx, edge(corev1.ServiceTypeNodePort)); got != "" {
		t.Errorf("a Gateway on node ports: reads %q", got)
	}
	if got := edgeClientAddressHeader(ctx, edge(corev1.ServiceTypeClusterIP)); got != "CF-Connecting-IP" {
		t.Errorf("a Gateway reachable through the tunnel alone: reads %q, want the tunnel's header", got)
	}
	if got := edgeClientAddressHeader(ctx, fake.NewClientBuilder().WithScheme(perimeterScheme()).Build()); got != "" {
		t.Errorf("no Gateway Service yet: reads %q, want the connection", got)
	}
	// What the operator was told outranks what it sees.
	t.Setenv("NETWORK_MODE", "static-ip")
	if got := edgeClientAddressHeader(ctx, edge(corev1.ServiceTypeClusterIP)); got != "" {
		t.Errorf("told static-ip: reads %q", got)
	}
	t.Setenv("PERIMETER_CLIENT_ADDRESS_HEADER", "True-Client-IP")
	if got := edgeClientAddressHeader(ctx, edge(corev1.ServiceTypeLoadBalancer)); got != "True-Client-IP" {
		t.Errorf("the administrator named a header: reads %q", got)
	}
}
