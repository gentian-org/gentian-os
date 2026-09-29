/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func platformTenantFixture() *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{}
	t.Name = "platform"
	t.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	return t
}

func acmeTenantFixture() *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{}
	t.Name = "acme"
	return t
}

// The platform tenant's desktop is the console, in the kernel zone: kernel
// realm, the kernel zone's client and cookie, a host on the kernel domain
// (AD-10). Every other tenant's desktop is in that tenant's own zone.
func TestTheDesktopsZoneFollowsTheTenantsRealm(t *testing.T) {
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	comp := &gentianov1alpha1.Component{}
	comp.Name = "desktop"
	e := &gentianov1alpha1.ExposureSpec{Name: "web", SubDomain: "console"}

	platform := r.zoneOf(platformTenantFixture())
	if !platform.kernel || platform.clientID != edgeKernelClientID || platform.cookie != edgeKernelAccessTokenCookie || platform.realm != "kernel" {
		t.Fatalf("platform zone = %+v", platform)
	}
	if got := exposureHost(platform, comp, e); got != "console.k.example" {
		t.Fatalf("platform console host = %q", got)
	}
	acme := r.zoneOf(acmeTenantFixture())
	if acme.kernel || acme.clientID != "gentian-edge-acme" || acme.realm != "acme" || acme.sectionName != tenantGatewayListenerName("acme") {
		t.Fatalf("acme zone = %+v", acme)
	}
	if got := exposureHost(acme, comp, e); got != "console.acme.k.example" {
		t.Fatalf("acme console host = %q", got)
	}
}

// What the platform tells the desktop, and nothing a profile author could
// know: behind the edge, which tenant, which zone client, where the director
// is, and no authority of its own.
func TestPlatformValuesLandWhereTheProfileSays(t *testing.T) {
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", Cluster: "c1"}
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Platform: &gentianov1alpha1.PlatformValueMapping{
			IssuerKey: "auth.issuer", ZoneClientIDKey: "auth.clientId", AudienceKey: "auth.audience",
			DirectorURLKey: "director.url", ClusterKey: "director.cluster",
			TenantKey: "tenant", ZoneKindKey: "zoneKind",
		},
	}
	tenant := platformTenantFixture()
	v := r.platformValues(profile, tenant, r.zoneOf(tenant))
	auth := v["auth"].(map[string]interface{})
	if auth["clientId"] != edgeKernelClientID || auth["audience"] != directorAudience {
		t.Fatalf("auth = %v", auth)
	}
	if auth["issuer"] != "https://id.k.example/auth/realms/kernel" {
		t.Fatalf("issuer = %v, want the zone's realm on the identity provider", auth["issuer"])
	}
	if v["tenant"] != "platform" || v["zoneKind"] != "kernel" {
		t.Fatalf("tenant/zoneKind = %v %v", v["tenant"], v["zoneKind"])
	}
	if v["director"].(map[string]interface{})["cluster"] != "c1" {
		t.Fatalf("director = %v", v["director"])
	}
	// A key the profile did not name is not set: the chart is told only what
	// it asked to be told.
	if _, present := v["kernelDomain"]; present {
		t.Fatal("kernelDomain was set without being asked for")
	}
}

// The database Secret's name lands as a string where the profile asks for the
// name, and as a structured reference where it asks for a host. A chart that
// consumes the Secret with envFrom needs the first; handing it the second
// names a map and nothing mounts, which is how the desktop would have lost its
// database the moment its profile mapped the wrong key.
func TestTheDatabaseSecretNameIsAStringWhereAskedFor(t *testing.T) {
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Database: &gentianov1alpha1.DatabaseValueMapping{
			SecretNameKey: "existingSecret.name",
			HostKey:       "database.host",
		},
	}
	v := databaseValues(profile, "desktop-database")
	if got := v["existingSecret"].(map[string]interface{})["name"]; got != "desktop-database" {
		t.Fatalf("existingSecret.name = %#v, want the plain name", got)
	}
	host := v["database"].(map[string]interface{})["host"].(map[string]interface{})
	if host["valueFrom"] != "desktop-database" {
		t.Fatalf("database.host = %#v, want a valueFrom reference", host)
	}
}

// A profile with no platform mapping receives nothing, and one that does not
// ask where the director is gets no egress to it.
func TestAProfileThatAsksForNothingGetsNothing(t *testing.T) {
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", Cluster: "c1"}
	profile := &gentianov1alpha1.ComponentProfile{}
	tenant := platformTenantFixture()
	if v := r.platformValues(profile, tenant, r.zoneOf(tenant)); len(v) != 0 {
		t.Fatalf("values = %v", v)
	}
	if wantsDirector(profile) {
		t.Fatal("no mapping, no director")
	}
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Platform: &gentianov1alpha1.PlatformValueMapping{DirectorURLKey: "director.url"},
	}
	if !wantsDirector(profile) {
		t.Fatal("naming the director key is asking for the director")
	}
}

// A component's route carries its L2 question for the table, attaches to
// the authenticated Gateway on the zone's listener, and names its backend in
// its own namespace.
func TestAComponentRouteCarriesItsQuestion(t *testing.T) {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace = "desktop", "tenant-platform"
	zone := edgeZone{domain: "k.example", cookie: edgeKernelAccessTokenCookie, sectionName: wildcardListenerName, kernel: true}
	e := &gentianov1alpha1.ExposureSpec{
		Name: "api", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC,
		SubDomain: "console", Paths: []string{"/api", "/healthz"}, ForwardToken: true,
		Backend: gentianov1alpha1.BackendRef{Service: "desktop-gentian-portal-api", Port: 8000},
	}
	route := buildExposureRoute(comp, "desktop-api", "console.k.example", zone, e, exposureAuthz(platformTenantFixture(), e.ForwardToken), "k.example")
	if route.Labels[edgeAuthzRouteLabel] != "true" || route.Annotations[edgeAuthzRelationAnnotation] != "can_enter" ||
		route.Annotations[edgeAuthzObjectAnnotation] != "tenant:platform" || route.Annotations[edgeAuthzForwardAnnotation] != "true" {
		t.Fatalf("route question = %v %v", route.Labels, route.Annotations)
	}
	if string(route.Spec.ParentRefs[0].Name) != AuthenticatedGatewayName || string(*route.Spec.ParentRefs[0].SectionName) != wildcardListenerName {
		t.Fatalf("parent = %+v", route.Spec.ParentRefs[0])
	}
	// Its two paths, and the code flow's landing path, which an entry that
	// does not cover the whole host would otherwise leave unroutable.
	if len(route.Spec.Rules) != 3 || string(route.Spec.Rules[0].BackendRefs[0].Name) != "desktop-gentian-portal-api" ||
		*route.Spec.Rules[2].Matches[0].Path.Value != edgeOAuth2Prefix {
		t.Fatalf("rules = %+v", route.Spec.Rules)
	}
	// A second route on the same host folds into the same table entry.
	web := buildExposureRoute(comp, "desktop-web", "console.k.example", zone,
		&gentianov1alpha1.ExposureSpec{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "console",
			Backend: gentianov1alpha1.BackendRef{Service: "desktop-gentian-portal-web", Port: 8080}},
		exposureAuthz(platformTenantFixture(), false), "k.example")

	// Every rule admits embedding by the kernel domain and nothing else: the
	// desktop opens components in frames, and nobody else may.
	for _, rule := range route.Spec.Rules {
		if len(rule.Filters) != 1 || rule.Filters[0].ResponseHeaderModifier == nil ||
			rule.Filters[0].ResponseHeaderModifier.Set[0].Value != "frame-ancestors 'self' https://*.k.example" {
			t.Fatalf("rule %v carries no frame policy", rule.Matches)
		}
	}

	// And the table reads it back, from any namespace.
	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(route, web).Build()
	entries, err := componentRouteTableEntries(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Host != "console.k.example" || entries[0].Relation != "can_enter" || !entries[0].ForwardToken || entries[0].AuthMode != "oidc" || entries[0].AccessTokenCookie != edgeKernelAccessTokenCookie {
		t.Fatalf("entries = %+v", entries)
	}
}

// denyPaths reaches the enforcement point. The CRD described the control for
// a while and no code read the field, so a component author could list an
// administrative path, believe it kept off the edge, and have it served.
//
// It is carried on the route and unioned per host, because two exposures
// share a host and deny wins: an entry that denies a path denies it for
// everything on that host.
func TestDenyPathsReachTheTableAndAreUnionedPerHost(t *testing.T) {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace = "odoo", "tenant-acme"
	zone := edgeZone{domain: "k.example", cookie: edgeKernelAccessTokenCookie, sectionName: wildcardListenerName}
	build := func(name, sub string, deny []string) *gatewayv1.HTTPRoute {
		e := &gentianov1alpha1.ExposureSpec{
			Name: name, Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC,
			SubDomain: sub, DenyPaths: deny,
			Backend: gentianov1alpha1.BackendRef{Service: "odoo", Port: 80},
		}
		return buildExposureRoute(comp, "odoo-"+name, sub+".k.example", zone, e,
			exposureAuthz(platformTenantFixture(), false), "k.example")
	}
	web := build("web", "shop", []string{"/web/database"})
	api := build("api", "shop", []string{"/admin"})
	other := build("wiki", "wiki", nil)

	if web.Annotations[edgeAuthzDenyPathsAnnotation] != "/web/database" {
		t.Fatalf("annotation = %q", web.Annotations[edgeAuthzDenyPathsAnnotation])
	}
	if _, has := other.Annotations[edgeAuthzDenyPathsAnnotation]; has {
		t.Fatal("an exposure that denies nothing must not carry the annotation")
	}

	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(web, api, other).Build()
	entries, err := componentRouteTableEntries(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	byHost := map[string][]string{}
	for _, e := range entries {
		byHost[e.Host] = e.DenyPaths
	}
	// Both exposures' entries, in the order the routes list: stable across
	// reconciles, which is what keeps the shim's ConfigMap from churning.
	got := append([]string(nil), byHost["shop.k.example"]...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "/admin" || got[1] != "/web/database" {
		t.Fatalf("shop denyPaths = %v, want both exposures' union", got)
	}
	if len(byHost["wiki.k.example"]) != 0 {
		t.Fatalf("wiki denyPaths = %v, want none", byHost["wiki.k.example"])
	}
}

// A policy in a component's namespace names the edge namespace for the
// zone's Secret and the shim, which the ReferenceGrant there admits.
func TestAZonePolicyOutsideTheEdgeNamesIt(t *testing.T) {
	zone := edgeZone{domain: "k.example", realm: "kernel", clientID: edgeKernelClientID, secretName: edgeKernelSecretName, cookie: "c", idCookie: "i"}
	spec := zoneSecurityPolicySpec("k.example", zone, "desktop-api", routeAuthz{forwardToken: true}, "kernel-edge", "gentian-os-edge-authz")
	// The secret is read from the policy's own namespace and no other
	// (Envoy Gateway 1.2), so it is named without one and copied beside
	// the policy; the shim is reached across namespaces under the grant.
	secret := spec["oidc"].(map[string]interface{})["clientSecret"].(map[string]interface{})
	if _, has := secret["namespace"]; has || secret["name"] != edgeKernelSecretName {
		t.Fatalf("clientSecret = %v", secret)
	}
	backend := spec["extAuth"].(map[string]interface{})["grpc"].(map[string]interface{})["backendRef"].(map[string]interface{})
	if backend["namespace"] != "kernel-edge" {
		t.Fatalf("backend = %v", backend)
	}
	// The kernel's own policies live in the edge namespace and name none.
	kernel := kernelSecurityPolicySpec("k.example", "kernel", "kernel-argocd", routeAuthz{}, "s")
	if _, has := kernel["oidc"].(map[string]interface{})["clientSecret"].(map[string]interface{})["namespace"]; has {
		t.Fatal("a policy in the edge namespace must not name it")
	}
	_ = metav1.Now()
}
