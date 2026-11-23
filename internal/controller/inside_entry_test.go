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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A Nextcloud that declares its API for the cluster's own programs: an
// inside entry, whose callers present an app password.
func nextcloudWithInside() *gentianov1alpha1.ComponentProfile {
	p := nextcloudBaseProfile()
	p.Spec.Expose = append(p.Spec.Expose, gentianov1alpha1.ExposureSpec{
		Name:      "api",
		Surface:   gentianov1alpha1.SurfaceInside,
		AuthMode:  gentianov1alpha1.AuthModeApp,
		SubDomain: "nc-api",
		Paths:     []string{"/ocs/"},
		Backend:   gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080},
	})
	return p
}

func insideApproved(kind gentianov1alpha1.ExposureKind) gentianov1alpha1.TenantExposure {
	return gentianov1alpha1.TenantExposure{
		Install: "nextcloud-base-ce", ExposureName: "api", Owner: "u-tom", Kind: kind,
		ExpiresAt: exposureEnds(time.Now().Add(24 * time.Hour)),
	}
}

func insideProfiles() map[string]*gentianov1alpha1.ComponentProfile {
	p := nextcloudWithInside()
	return map[string]*gentianov1alpha1.ComponentProfile{p.Name: p}
}

// The inside Gateway has one listener per approved inside host, of its own
// class, and is not built at all while no tenant has one: a Gateway needs a
// listener, and its fleet starts with it.
func TestTheInsideGatewayListensOnTheApprovedInsideHostsAlone(t *testing.T) {
	tenant := acmeTenantFixture()
	if gw := buildInsideGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, insideProfiles()); gw != nil {
		t.Fatalf("an inside Gateway with nothing approved: %+v", gw.Spec.Listeners)
	}
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{insideApproved(gentianov1alpha1.ExposureKindPublicAppCredential)}
	if gw := buildInsideGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, insideProfiles()); gw != nil {
		t.Fatal("an approval of another kind gave the inside entry a listener")
	}

	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{insideApproved(gentianov1alpha1.ExposureKindInsideAppCredential)}
	gw := buildInsideGateway("k.example", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, insideProfiles())
	if gw == nil {
		t.Fatal("no inside Gateway for an approved inside entry")
	}
	if gw.Name != InsideGatewayName || gw.Namespace != servicesNamespace || string(gw.Spec.GatewayClassName) != InsideGatewayClassName {
		t.Fatalf("inside Gateway %s/%s of class %s", gw.Namespace, gw.Name, gw.Spec.GatewayClassName)
	}
	if len(gw.Spec.Listeners) != 1 {
		t.Fatalf("listeners %+v, want the one inside host", gw.Spec.Listeners)
	}
	l := gw.Spec.Listeners[0]
	if l.Hostname == nil || string(*l.Hostname) != "nc-api.acme.k.example" || l.Port != 443 ||
		string(l.Name) != insideListenerName("nc-api.acme.k.example") {
		t.Fatalf("listener %+v, want https on nc-api.acme.k.example", l)
	}
	if ref := l.TLS.CertificateRefs[0]; string(ref.Name) != tenantWildcardSecretName("acme") {
		t.Fatalf("certificate %s, want the tenant's wildcard", ref.Name)
	}

	// Never on the edge: the perimeter Gateway has no listener for it.
	for _, pl := range buildPerimeterGateway("k.example", "", "", "kernel", []gentianov1alpha1.Tenant{*tenant}, insideProfiles()).Spec.Listeners {
		if pl.Hostname != nil && string(*pl.Hostname) == "nc-api.acme.k.example" {
			t.Fatal("the inside host has a listener on the public perimeter")
		}
	}
}

// The inside Gateway carries none of the annotations that tell external-dns
// where the edge answers, and goes when the last inside entry does.
func TestTheInsideGatewayIsNotPublishedAndGoesWithTheLastEntry(t *testing.T) {
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	tenant.Spec.Exposures = []gentianov1alpha1.TenantExposure{insideApproved(gentianov1alpha1.ExposureKindInsideAppCredential)}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, nextcloudWithInside()).Build()
	tunnel := &CloudflareTunnelIngress{tunnelCNAME: "abc.cfargotunnel.com"}
	r := &GatewayPlatformReconciler{Client: c, KernelDomain: "k.example", Ingress: tunnel}
	ctx := context.Background()

	if err := r.ensureEdgeGateways(ctx); err != nil {
		t.Fatal(err)
	}
	edge := &gatewayv1.Gateway{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: servicesNamespace, Name: PerimeterGatewayName}, edge); err != nil {
		t.Fatal(err)
	}
	if edge.Annotations[edgeDNSTargetKey] == "" {
		t.Fatal("the test's ingress put no DNS annotation on the edge; it asks nothing")
	}
	inside := &gatewayv1.Gateway{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: servicesNamespace, Name: InsideGatewayName}, inside); err != nil {
		t.Fatalf("no inside Gateway: %v", err)
	}
	for k := range inside.Annotations {
		if strings.HasPrefix(k, "external-dns.") {
			t.Fatalf("the inside Gateway carries %s", k)
		}
	}

	tenant.Spec.Exposures = nil
	if err := c.Update(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureEdgeGateways(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: servicesNamespace, Name: InsideGatewayName}, inside); err == nil {
		t.Fatal("the inside Gateway stayed with no inside entry approved")
	}
}

// An approved inside entry is served by a publishing proxy as a perimeter
// entry is, with three differences: its route is on the inside Gateway and
// tells external-dns to leave it out, its proxy admits the inside fleet and
// not the edge, and the caller's address is the connection's.
func TestAnInsideEntryIsRoutedOnTheInsideGatewayAlone(t *testing.T) {
	t.Setenv("KERNEL_NETWORK_POLICIES", "true")
	t.Setenv("NETWORK_MODE", "tunnel")
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	dmz := layout.TenantDMZ(tenant.Name)
	comp := componentFor("nextcloud-base-ce", tenantNamespaceName(tenant))
	comp.Spec.Exposures = []gentianov1alpha1.ExposureEnablement{{
		ExposureName: "api", Owner: "u-tom", Kind: gentianov1alpha1.ExposureKindInsideAppCredential,
		ExpiresAt: exposureEnds(time.Now().Add(24 * time.Hour)), Alias: "api",
	}}
	profile := nextcloudWithInside()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), profile, comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	ctx := context.Background()

	public, inside, err := r.ensurePerimeterSurfaces(ctx, comp, profile, tenant, r.zoneOf(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if public != 0 || inside != 1 {
		t.Fatalf("published %d on the perimeter and %d inside, want 0 and 1", public, inside)
	}
	name := perimeterName(comp, "api")

	route := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: dmz, Name: name}, route); err != nil {
		t.Fatalf("no route: %v", err)
	}
	if !routeOnInside(route) || routeOnEdge(route) {
		t.Fatalf("route parents %+v, want the inside Gateway alone", route.Spec.ParentRefs)
	}
	if got := string(*route.Spec.ParentRefs[0].SectionName); got != insideListenerName("nc-api.acme.k.example") {
		t.Fatalf("route section %s", got)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "nc-api.acme.k.example" {
		t.Fatalf("route hosts %v, want the inside host and no alias", route.Spec.Hostnames)
	}
	if route.Annotations["external-dns.alpha.kubernetes.io/controller"] != "none" {
		t.Fatalf("route annotations %v: external-dns would publish the inside host", route.Annotations)
	}

	front := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: dmz, Name: name + "-ingress"}, front); err != nil {
		t.Fatalf("no ingress policy on the proxy: %v", err)
	}
	peer := front.Spec.Ingress[0].From[0]
	if peer.PodSelector.MatchLabels["gateway.envoyproxy.io/owning-gatewayclass"] != netpolicy.InsideGatewayClassName ||
		peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != layout.Namespace(layout.Edge) {
		t.Fatalf("the proxy admits %+v, want the inside fleet alone", peer)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: dmz, Name: name}, cm); err != nil {
		t.Fatal(err)
	}
	conf := cm.Data["nginx.conf"]
	if strings.Contains(strings.ToLower(conf), "cf_connecting_ip") {
		t.Fatal("behind the inside Gateway the proxy reads a header a caller writes for its address")
	}
	if strings.Contains(conf, `proxy_set_header Authorization "";`) {
		t.Fatal("the caller's credential does not reach the app on an approved inside entry")
	}

	// A perimeter entry's route stays where it was, with no such annotation.
	pm := &perimeterEnablement{spec: &profile.Spec.Expose[0], host: "share.acme.k.example"}
	if r := perimeterRoute("x", dmz, nil, pm); !routeOnEdge(r) || len(r.Annotations) != 0 {
		t.Fatalf("a perimeter route on %+v with %v", r.Spec.ParentRefs, r.Annotations)
	}
}

// The hairpin names an inside host at the inside Gateway's address, and
// leaves it out while that Service has none. A host the edge serves stays
// the edge's.
func TestTheHairpinMapsInsideHostsToTheInsideGateway(t *testing.T) {
	edgeHosts := map[string]struct{}{"cloud.acme.k.example": {}}
	insideHosts := map[string]struct{}{"nc-api.acme.k.example": {}, "cloud.acme.k.example": {}}

	got := hairpinAddresses("10.0.0.1", "k.example", edgeHosts, "10.0.0.2", insideHosts)
	if got["nc-api.acme.k.example"] != "10.0.0.2" {
		t.Fatalf("inside host at %q, want the inside Gateway's 10.0.0.2", got["nc-api.acme.k.example"])
	}
	if got["cloud.acme.k.example"] != "10.0.0.1" || got["id.k.example"] != "10.0.0.1" {
		t.Fatalf("edge hosts moved: %v", got)
	}
	corefile, changed := patchHairpinCorefile(".:53 {\n    hosts {\n      fallthrough\n    }\n}", "k.example", got)
	if !changed || !strings.Contains(corefile, "10.0.0.2 nc-api.acme.k.example") || !strings.Contains(corefile, "10.0.0.1 id.k.example") {
		t.Fatalf("Corefile:\n%s", corefile)
	}
	if hairpinHosts(corefile)["nc-api.acme.k.example"] != "10.0.0.2" {
		t.Fatal("the block read back does not give the inside host its address")
	}

	got = hairpinAddresses("10.0.0.1", "k.example", edgeHosts, "", insideHosts)
	if _, ok := got["nc-api.acme.k.example"]; ok {
		t.Fatal("an inside host was named with no address for it")
	}
	// Once named, it goes again when the address does.
	again, changed := patchHairpinCorefile(corefile, "k.example", got)
	if !changed || strings.Contains(again, "nc-api.acme.k.example") {
		t.Fatalf("the inside host stayed without an address:\n%s", again)
	}
}

// The hairpin's routes: an inside route's hosts are the inside Gateway's,
// apart from the edge's.
func TestTheInsideRoutesHostsAreCollectedApart(t *testing.T) {
	scheme := perimeterScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		hairpinTestRoute("tenant-acme-dmz", "api", edgeTestParentRef(InsideGatewayName), "nc-api.acme.k.example"),
		hairpinTestRoute("tenant-acme-dmz", "share", edgeTestParentRef(PerimeterGatewayName), "share.acme.k.example"),
	).Build()
	edge, inside, err := collectRouteHairpinHosts(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inside["nc-api.acme.k.example"]; !ok || len(inside) != 1 {
		t.Fatalf("inside hosts %v", inside)
	}
	if _, ok := edge["nc-api.acme.k.example"]; ok {
		t.Fatal("an inside host was collected as the edge's")
	}
	if _, ok := edge["share.acme.k.example"]; !ok {
		t.Fatalf("edge hosts %v", edge)
	}
}

// A granted inside host opens the inside fleet's pods, by that Service's
// selector and on its target port, and not the edge's; a granted host of the
// edge opens the edge's. It never goes to the outgoing gateway.
func TestAGrantedInsideHostOpensTheInsideFleet(t *testing.T) {
	svc := func(name, ip, class string) corev1.Service {
		return corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kernel-edge", Name: name},
			Spec: corev1.ServiceSpec{
				ClusterIP: ip,
				Selector:  map[string]string{"app.kubernetes.io/name": "envoy", "gateway.envoyproxy.io/owning-gatewayclass": class},
				Ports:     []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromInt32(10443)}},
			},
		}
	}
	services := []corev1.Service{svc("edge", "10.0.0.1", GentianGatewayClassName), svc("inside", "10.0.0.2", InsideGatewayClassName)}
	own := hairpinHosts("# BEGIN gentian-hairpin\n10.0.0.1 store.k.example\n10.0.0.2 nc-api.acme.k.example\n# END gentian-hairpin")

	outside, inside := splitOwnHosts([]string{"api.stripe.com", "nc-api.acme.k.example"}, own)
	if len(outside) != 1 || outside[0] != "api.stripe.com" || len(inside) != 1 {
		t.Fatalf("outside %v, inside %v", outside, inside)
	}
	rules := ownHostsRulesFor(services, inside, own)
	if len(rules) != 1 {
		t.Fatalf("rules %+v, want one to the inside fleet", rules)
	}
	if rules[0].To[0].PodSelector.MatchLabels["gateway.envoyproxy.io/owning-gatewayclass"] != InsideGatewayClassName ||
		rules[0].Ports[0].Port.IntVal != 10443 {
		t.Fatalf("rule %+v, want the inside fleet on 10443", rules[0])
	}

	rules = ownHostsRulesFor(services, []string{"store.k.example", "nc-api.acme.k.example"}, own)
	if len(rules) != 2 {
		t.Fatalf("rules %+v, want one to each fleet", rules)
	}
	// An address that is neither Service's opens nothing.
	if got := ownHostsRulesFor(services, []string{"x.k.example"}, map[string]string{"x.k.example": "10.9.9.9"}); len(got) != 0 {
		t.Fatalf("a host at nobody's address opened %+v", got)
	}
}
