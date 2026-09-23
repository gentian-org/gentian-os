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
func TestDesktopValuesAreThePlatformsFacts(t *testing.T) {
	r := &ComponentReconciler{KernelDomain: "k.example", KernelRealm: "kernel", Cluster: "c1"}
	v := r.desktopValues(platformTenantFixture(), r.zoneOf(platformTenantFixture()), "desktop-database")
	auth := v["auth"].(map[string]interface{})
	if auth["mode"] != "edge" || auth["clientId"] != edgeKernelClientID {
		t.Fatalf("auth = %v", auth)
	}
	if v["tenant"] != "platform" || v["existingSecret"].(map[string]interface{})["name"] != "desktop-database" {
		t.Fatalf("tenant/secret = %v %v", v["tenant"], v["existingSecret"])
	}
	if v["rbac"].(map[string]interface{})["create"] != false {
		t.Fatal("the desktop holds no Kubernetes authority")
	}
	if v["director"].(map[string]interface{})["cluster"] != "c1" {
		t.Fatalf("director = %v", v["director"])
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
	route := buildExposureRoute(comp, "desktop-api", "console.k.example", zone, e, exposureAuthz(platformTenantFixture(), e.ForwardToken))
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
		exposureAuthz(platformTenantFixture(), false))

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
