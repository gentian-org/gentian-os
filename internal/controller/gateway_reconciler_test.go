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
	"maps"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func TestNormalizeRoutingMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", RoutingModeGateway},
		{"ingress", RoutingModeGateway},
		{"GATEWAY", RoutingModeGateway},
		{" gateway ", RoutingModeGateway},
		{"nginx", RoutingModeGateway},
	}
	for _, tc := range tests {
		if got := normalizeRoutingMode(tc.in); got != tc.want {
			t.Fatalf("normalizeRoutingMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildKernelGateway(t *testing.T) {
	t.Parallel()
	tenants := []gentianov1alpha1.Tenant{
		{ObjectMeta: metav1.ObjectMeta{Name: "demo"}},
	}
	gw := buildAuthenticatedGateway("platform.example.test", "multi", tenants)
	if gw.Name != AuthenticatedGatewayName {
		t.Fatalf("name = %q", gw.Name)
	}
	if gw.Namespace != servicesNamespace {
		t.Fatalf("namespace = %q", gw.Namespace)
	}
	if string(gw.Spec.GatewayClassName) != GentianGatewayClassName {
		t.Fatalf("gatewayClassName = %q", gw.Spec.GatewayClassName)
	}
	// Looked up by name rather than by index. These assertions used to be
	// positional, so adding the :80 http-redirect listener — which buildGateway
	// inserts second, before the caller's extraListeners — shifted https-apex and
	// every tenant listener down one and failed the test on a count mismatch
	// rather than on anything meaningful. Names are the stable identity here.
	byName := map[string]gatewayv1.Listener{}
	for _, l := range gw.Spec.Listeners {
		byName[string(l.Name)] = l
	}
	if len(gw.Spec.Listeners) != len(byName) {
		t.Fatalf("duplicate listener names in %d listeners", len(gw.Spec.Listeners))
	}
	// The kernel HTTPS listener carries NO hostname on purpose.
	//
	// A listener's hostname both selects it by SNI and gates which routes may
	// attach, and a route only attaches where its hostnames intersect. The
	// kernel certificate covers platform.example.test and *.platform.example.test, so a
	// browser may coalesce console.platform.example.test onto an existing
	// platform.example.test connection; with a listener scoped to the apex that
	// request had nowhere to attach and Envoy returned a bare 404. Serving every
	// name the certificate covers from one listener removes the hole.
	wildcard, ok := byName["https-wildcard"]
	if !ok {
		t.Fatalf("listener https-wildcard missing; have %v", slices.Sorted(maps.Keys(byName)))
	}
	if wildcard.Hostname != nil {
		t.Fatalf("kernel HTTPS listener hostname = %q, want none so coalesced requests still route", *wildcard.Hostname)
	}
	if _, exists := byName["https-apex"]; exists {
		t.Fatal("https-apex listener still present: the catch-all serves the apex, and a narrow apex listener makes every other host unroutable on connections coalesced onto it")
	}
	// Tenant subdomains need the tenant certificate, so they keep a listener.
	tenantL, ok := byName["https-tenant-demo-wildcard"]
	if !ok {
		t.Fatalf("listener https-tenant-demo-wildcard missing; have %v", slices.Sorted(maps.Keys(byName)))
	}
	if tenantL.Hostname == nil || string(*tenantL.Hostname) != "*.demo.platform.example.test" {
		t.Fatalf("tenant listener hostname = %v", tenantL.Hostname)
	}
	if _, exists := byName["https-tenant-demo-apex"]; exists {
		t.Fatal("https-tenant-demo-apex still present: the tenant apex is covered by the kernel certificate and served by the catch-all")
	}

	// :80 is not this Gateway's. Under mergeGateways a listener is unique per
	// port and hostname across the class, and :80 -- the ACME challenge and
	// the https redirect -- is the perimeter's (networking.md §1).
	if _, exists := byName[httpRedirectListenerName]; exists {
		t.Fatalf("listener %q on the authenticated Gateway; :80 belongs to the perimeter", httpRedirectListenerName)
	}
	perimeter := buildPerimeterGateway("platform.example.test", "", "kernel", nil, nil)
	if perimeter.Name != PerimeterGatewayName || perimeter.Namespace != servicesNamespace {
		t.Fatalf("perimeter Gateway = %s/%s", perimeter.Namespace, perimeter.Name)
	}
	perimeterByName := map[string]gatewayv1.Listener{}
	for _, l := range perimeter.Spec.Listeners {
		perimeterByName[string(l.Name)] = l
	}
	// The :80 redirect listener is hostname-less on purpose: it must match every
	// host so any plaintext request can be bounced to https.
	redirect, ok := perimeterByName[httpRedirectListenerName]
	if !ok {
		t.Fatalf("listener %q missing on the perimeter; have %v", httpRedirectListenerName, slices.Sorted(maps.Keys(perimeterByName)))
	}
	if redirect.Port != 80 {
		t.Fatalf("redirect listener port = %d, want 80", redirect.Port)
	}
	if redirect.Hostname != nil {
		t.Fatalf("redirect listener hostname = %v, want nil (match all hosts)", *redirect.Hostname)
	}
	// The identity provider's own hostname, with the kernel wildcard certificate.
	idL, ok := perimeterByName[perimeterIDListenerName]
	if !ok {
		t.Fatalf("listener %q missing on the perimeter; have %v", perimeterIDListenerName, slices.Sorted(maps.Keys(perimeterByName)))
	}
	if idL.Hostname == nil || string(*idL.Hostname) != "id.platform.example.test" {
		t.Fatalf("id listener hostname = %v", idL.Hostname)
	}
	if idL.TLS == nil || len(idL.TLS.CertificateRefs) != 1 || string(idL.TLS.CertificateRefs[0].Name) != kernelWildcardTLSSecretName {
		t.Fatalf("id listener certificate = %v, want %s", idL.TLS, kernelWildcardTLSSecretName)
	}

	if wildcard.AllowedRoutes == nil || wildcard.AllowedRoutes.Namespaces.From == nil ||
		*wildcard.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromAll {
		t.Fatalf("kernel gateway should allow cross-namespace routes, got %v", wildcard.AllowedRoutes)
	}
	if string(wildcard.TLS.CertificateRefs[0].Name) != kernelWildcardTLSSecretName {
		t.Fatalf("tls secret = %q", wildcard.TLS.CertificateRefs[0].Name)
	}
}

func TestGatewayProgrammed(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("install gateway scheme: %v", err)
	}

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: AuthenticatedGatewayName, Namespace: layout.Namespace(layout.Edge)},
	}
	gw.Status.Conditions = []metav1.Condition{
		{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, Reason: "Programmed"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw).Build()
	ok, reason := gatewayProgrammed(t.Context(), c, gw)
	if !ok || reason != "Programmed" {
		t.Fatalf("expected programmed gateway, got ok=%v reason=%q", ok, reason)
	}
}

func TestGatewayProgrammedAddressNotAssignedWithListeners(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("install gateway scheme: %v", err)
	}

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: AuthenticatedGatewayName, Namespace: layout.Namespace(layout.Edge)},
	}
	gw.Status.Conditions = []metav1.Condition{
		{
			Type:   string(gatewayv1.GatewayConditionProgrammed),
			Status: metav1.ConditionFalse,
			Reason: "AddressNotAssigned",
		},
	}
	gw.Status.Listeners = []gatewayv1.ListenerStatus{
		{
			Name: "https-wildcard",
			Conditions: []metav1.Condition{
				{Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, Reason: "Programmed"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw).Build()
	ok, reason := gatewayProgrammed(t.Context(), c, gw)
	if !ok || reason != "ListenersProgrammed" {
		t.Fatalf("expected listeners-programmed gateway, got ok=%v reason=%q", ok, reason)
	}
}

func TestTenantGatewayName(t *testing.T) {
	t.Parallel()
	if got := tenantGatewayName("demo"); got != "tenant-demo-gateway" {
		t.Fatalf("got %q", got)
	}
}

func TestComputeGatewayFrameAncestorsPolicy(t *testing.T) {
	t.Parallel()
	policy := computeGatewayFrameAncestorsPolicy("platform.example.test", "demo.platform.example.test", "app")
	if policy.Mode != gatewayFrameAncestorsReplace {
		t.Fatalf("mode = %q", policy.Mode)
	}
	if policy.Origins != "https://console.platform.example.test https://console.demo.platform.example.test https://*.demo.platform.example.test" {
		t.Fatalf("origins = %q", policy.Origins)
	}
}

func TestIngressGatewayFrameAncestorsPolicy(t *testing.T) {
	t.Parallel()
	ingress := &gentianov1alpha1.ExposureSpec{
		Annotations: map[string]string{
			gentianov1alpha1.AnnotationIngressGatewayFrameAncestors: `{"mode":"replace","origins":["mainApp","portal"]}`,
		},
	}
	policy, ok, err := ingressFrameAncestorsPolicy("platform.example.test", "demo.platform.example.test", "cloud", ingress)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected custom policy")
	}
	if policy.Mode != gatewayFrameAncestorsReplace {
		t.Fatalf("mode = %q", policy.Mode)
	}
	if !strings.Contains(policy.Origins, "https://cloud.demo.platform.example.test") {
		t.Fatalf("origins = %q", policy.Origins)
	}
	if !strings.Contains(policy.Origins, "https://console.platform.example.test") {
		t.Fatalf("origins = %q", policy.Origins)
	}
	// The tenant's own console is the host a tenant user is normally signed
	// in on. Leaving it out passes every server-side check and still blocks
	// the iframe in the browser, so assert it explicitly.
	if !strings.Contains(policy.Origins, "https://console.demo.platform.example.test") {
		t.Fatalf("origins = %q", policy.Origins)
	}
}

// The "portal" token must resolve to the same hosts the portal is actually
// routed on, so a policy that opts out of the computed default does not silently
// carry a narrower list than the default it replaced.
func TestIngressGatewayFrameAncestorsPortalTokenMatchesRoutedPortalHosts(t *testing.T) {
	t.Parallel()
	ingress := &gentianov1alpha1.ExposureSpec{
		Annotations: map[string]string{
			gentianov1alpha1.AnnotationIngressGatewayFrameAncestors: `{"mode":"replace","origins":["portal"]}`,
		},
	}
	policy, ok, err := ingressFrameAncestorsPolicy("platform.example.test", "demo.platform.example.test", "cloud", ingress)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected custom policy")
	}
	want := strings.Join(consoleOrigins("platform.example.test", "demo.platform.example.test"), " ")
	if policy.Origins != want {
		t.Fatalf("origins = %q, want %q", policy.Origins, want)
	}
}

// mainApp and portal collapse to one origin when the app is the tenant apex;
// a repeated origin in the header is noise, not a second permission.
func TestIngressGatewayFrameAncestorsDeduplicatesOrigins(t *testing.T) {
	t.Parallel()
	ingress := &gentianov1alpha1.ExposureSpec{
		Annotations: map[string]string{
			gentianov1alpha1.AnnotationIngressGatewayFrameAncestors: `{"mode":"replace","origins":["mainApp","portal"]}`,
		},
	}
	policy, _, err := ingressFrameAncestorsPolicy("platform.example.test", "demo.platform.example.test", "@", ingress)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(policy.Origins, "https://demo.platform.example.test"); got != 1 {
		t.Fatalf("origins = %q, want demo apex once", policy.Origins)
	}
}

func TestBackendTrafficPolicySpecFromIngressAnnotations(t *testing.T) {
	t.Parallel()
	spec := backendTrafficPolicySpecFromIngressAnnotations(map[string]string{
		gentianov1alpha1.AnnotationIngressGatewayRequestTimeout: "3600",
		gentianov1alpha1.AnnotationIngressGatewayBufferLimit:    "128m",
	})
	if spec == nil {
		t.Fatal("expected spec")
	}
	timeout, ok := spec["timeout"].(map[string]interface{})
	if !ok {
		t.Fatalf("timeout = %T", spec["timeout"])
	}
	http, ok := timeout["http"].(map[string]interface{})
	if !ok || http["requestTimeout"] != "3600s" {
		t.Fatalf("requestTimeout = %v", http["requestTimeout"])
	}
	conn, ok := spec["connection"].(map[string]interface{})
	if !ok || conn["bufferLimit"] != "128m" {
		t.Fatalf("bufferLimit = %v", conn["bufferLimit"])
	}
}

func TestAppAPIBackendRulesApplyEmbeddingFilters(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				gentianov1alpha1.AnnotationProfileGatewayAPIBackends: `[{"pathPrefix":"/portal-bridge","serviceName":"app-portal-bridge","port":8080}]`,
			},
		},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Expose: []gentianov1alpha1.ExposureSpec{{
				Name: "web", Surface: gentianov1alpha1.SurfaceGateway,
				AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "projects",
			}},
		},
	}
	ingress := &gentianov1alpha1.ExposureSpec{SubDomain: "projects"}
	rules := appAPIBackendRules(profile, 8080, "platform.example.test", "demo.platform.example.test", ingress)
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	if len(rules[0].Filters) != 1 {
		t.Fatalf("filters = %+v", rules[0].Filters)
	}
	modifier := rules[0].Filters[0].ResponseHeaderModifier
	if modifier == nil || len(modifier.Set) != 1 {
		t.Fatalf("modifier = %+v", modifier)
	}
	if !strings.Contains(modifier.Set[0].Value, "https://console.platform.example.test") {
		t.Fatalf("csp = %q", modifier.Set[0].Value)
	}
}

func TestBuildTenantReferenceGrantObjects(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	objects := buildTenantReferenceGrantObjects(tenant)
	if len(objects) != 2 {
		t.Fatalf("object count = %d, want 2", len(objects))
	}
	if objects[0].GetName() != "allow-tenant-routes-demo" {
		t.Fatalf("services RG name = %q", objects[0].GetName())
	}
	if objects[0].GetNamespace() != servicesNamespace {
		t.Fatalf("services RG namespace = %q", objects[0].GetNamespace())
	}
	if objects[1].GetName() != "allow-kernel-gateway-tls" {
		t.Fatalf("tenant RG name = %q", objects[1].GetName())
	}
	if objects[1].GetNamespace() != "tenant-demo" {
		t.Fatalf("tenant RG namespace = %q", objects[1].GetNamespace())
	}
}

func TestKernelHTTPRouteSpecs(t *testing.T) {
	t.Parallel()
	specs := kernelHTTPRouteSpecs("platform.example.test", []string{"demo.platform.example.test"}, nil, []string{"demo"}, false, "c1", true, true)
	// One route per kernel host, plus one per tenant apex sending the browser
	// to that tenant's console. Asserted by name rather than by count, so
	// adding a route does not fail a test that has nothing to do with it.
	byName := map[string]kernelHTTPRouteSpec{}
	for _, s := range specs {
		byName[s.name] = s
	}
	for _, want := range []string{
		kernelRouteKeycloakIDP, kernelRouteKeycloakRefused, kernelRouteKeycloakAdmin, kernelRouteWWWRedirect,
		kernelRouteArgoCD, kernelRouteHTTPRedirect, "tenant-demo-apex",
	} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("missing kernel route %q; got %v", want, specs)
		}
	}
	// The identity provider is a perimeter surface: its own listener on the
	// perimeter Gateway, realm endpoints and theme assets allowed, the master
	// realm and the admin console refused by a rule rather than by absence.
	idRoute := buildKernelHTTPRoute(byName[kernelRouteKeycloakIDP])
	if string(idRoute.Spec.Hostnames[0]) != "id.platform.example.test" {
		t.Fatalf("id host = %v", idRoute.Spec.Hostnames[0])
	}
	if got := string(idRoute.Spec.ParentRefs[0].Name); got != PerimeterGatewayName {
		t.Fatalf("id route parent = %q, want the perimeter Gateway", got)
	}
	if got := string(*idRoute.Spec.ParentRefs[0].SectionName); got != perimeterIDListenerName {
		t.Fatalf("id route listener = %q", got)
	}
	allowed := map[string]bool{}
	for _, rule := range idRoute.Spec.Rules {
		prefix := *rule.Matches[0].Path.Value
		if got := *rule.BackendRefs[0].Port; got != gatewayv1.PortNumber(8080) {
			t.Fatalf("id backend port = %d, want 8080 (Suze Keycloak)", got)
		}
		if ns := rule.BackendRefs[0].Namespace; ns == nil || string(*ns) != identityNamespace {
			t.Fatalf("id backend namespace = %v, want %s", ns, identityNamespace)
		}
		allowed[prefix] = true
	}
	for _, want := range []string{"/auth/realms/", "/auth/resources/"} {
		if !allowed[want] {
			t.Errorf("id.<kernel> must serve %s; served %v", want, allowed)
		}
	}
	// What it refuses is a route of its own -- more specific, so ranked
	// first -- closed by a policy that denies every caller.
	refused := byName[kernelRouteKeycloakRefused]
	if refused.gateway != PerimeterGatewayName || refused.host != "id.platform.example.test" {
		t.Fatalf("refused route = %+v", refused)
	}
	refusedPrefixes := map[string]bool{}
	for _, rule := range refused.rules {
		refusedPrefixes[*rule.Matches[0].Path.Value] = true
	}
	// The master realm only. /auth/admin/ used to be refused here too, when
	// the console lived on a hostname of its own; it is now a route on this
	// same host behind the kernel session, and refusing it here would close
	// the console to everyone.
	if !refusedPrefixes["/auth/realms/master/"] {
		t.Errorf("id.<kernel> must refuse the master realm; refused %v", refusedPrefixes)
	}
	if refusedPrefixes["/auth/admin/"] {
		t.Error("/auth/admin/ is behind the session now, not refused outright")
	}
	if auth, _ := refused.securityPolicy["authorization"].(map[string]interface{}); auth["defaultAction"] != "Deny" {
		t.Fatalf("the refused route's policy = %v, want defaultAction Deny", refused.securityPolicy)
	}
	// The admin console shares the issuer's hostname, because Keycloak
	// refuses its own Admin REST API when served on a second one. So it is
	// the same host and the same listener as the public realm routes, told
	// apart by path, and it is the one route there that carries a session.
	adminRoute := buildKernelHTTPRoute(byName[kernelRouteKeycloakAdmin])
	if string(adminRoute.Spec.Hostnames[0]) != "id.platform.example.test" {
		t.Fatalf("admin console host = %v, want the issuer's", adminRoute.Spec.Hostnames[0])
	}
	if got := string(adminRoute.Spec.ParentRefs[0].Name); got != PerimeterGatewayName {
		t.Fatalf("admin console parent = %q, want the same Gateway the issuer is on", got)
	}
	adminPaths := map[string]bool{}
	for _, rule := range byName[kernelRouteKeycloakAdmin].rules {
		adminPaths[*rule.Matches[0].Path.Value] = true
	}
	if !adminPaths["/auth/admin/"] || !adminPaths[edgeOAuth2Prefix] {
		t.Fatalf("admin console paths = %v, want /auth/admin/ and the callback", adminPaths)
	}
	if byName[kernelRouteKeycloakAdmin].authz == nil ||
		byName[kernelRouteKeycloakAdmin].authz.relation != "can_configure" {
		t.Fatal("the admin console must be behind can_configure")
	}
	// And the public route on that host must not answer for it.
	for _, rule := range byName[kernelRouteKeycloakIDP].rules {
		if *rule.Matches[0].Path.Value == "/auth/admin/" {
			t.Fatal("the public allowlist must not carry /auth/admin/")
		}
	}
	// The :80 redirect lives where :80 does, on the perimeter.
	redirect := buildKernelHTTPRoute(byName[kernelRouteHTTPRedirect])
	if got := string(redirect.Spec.ParentRefs[0].Name); got != PerimeterGatewayName {
		t.Fatalf("http redirect parent = %q, want the perimeter Gateway", got)
	}
	// www is an alias of the bare domain, by redirect; a tenant's apex is an
	// alias of the tenant's own console.
	www := buildKernelHTTPRoute(byName[kernelRouteWWWRedirect])
	if string(www.Spec.Hostnames[0]) != "www.platform.example.test" {
		t.Fatalf("www host = %v", www.Spec.Hostnames[0])
	}
	if got := *www.Spec.Rules[0].Filters[0].RequestRedirect.Hostname; string(got) != "platform.example.test" {
		t.Fatalf("www redirects to %q", got)
	}
	apex := buildKernelHTTPRoute(byName["tenant-demo-apex"])
	if got := *apex.Spec.Rules[0].Filters[0].RequestRedirect.Hostname; string(got) != "console.demo.platform.example.test" {
		t.Fatalf("tenant apex redirects to %q", got)
	}
}

// A redirect to a console that does not exist is a public dead end, so
// without a desktop profile no name is sent to one -- and before the kernel
// zone exists there is no platform console to send the kernel names to.
func TestNoConsoleRedirectsWithoutADesktop(t *testing.T) {
	t.Parallel()
	specs := kernelHTTPRouteSpecs("platform.example.test",
		[]string{"demo.platform.example.test"}, nil, []string{"demo"}, false, "c1", true, false)
	for _, s := range specs {
		switch s.name {
		case "tenant-demo-apex":
			t.Errorf("route %q published with no desktop behind it", s.name)
		}
	}
	specs = kernelHTTPRouteSpecs("platform.example.test",
		[]string{"demo.platform.example.test"}, nil, []string{"demo"}, false, "c1", false, true)
	// The routes that do not depend on the desktop are still there.
	var sawIDP bool
	for _, s := range specs {
		if s.name == kernelRouteKeycloakIDP {
			sawIDP = true
		}
	}
	if !sawIDP {
		t.Error("the identity route should not wait for the desktop")
	}
}

func TestKernelHTTPRouteSpecsLLMDisabledByDefault(t *testing.T) {
	specs := kernelHTTPRouteSpecs("platform.example.test", []string{"demo.platform.example.test"}, nil, []string{"demo"}, false, "c1", true, true)
	for _, spec := range specs {
		if spec.name == kernelRouteLiteLLM {
			t.Fatalf("kernel-llm route present with llm disabled")
		}
	}
}

func TestKernelHTTPRouteSpecsLLMEnabled(t *testing.T) {
	specs := kernelHTTPRouteSpecs("platform.example.test", nil, nil, nil, true, "c1", true, true)
	// By name, not by count: adding a kernel route should not fail a test
	// about the LLM one. The LLM route is still appended last, which is what
	// the specs[len-1] lookup below relies on.
	byName := map[string]struct{}{}
	for _, s := range specs {
		byName[s.name] = struct{}{}
	}
	for _, want := range []string{
		kernelRouteKeycloakIDP, kernelRouteWWWRedirect,
		kernelRouteArgoCD, kernelRouteHTTPRedirect, kernelRouteLiteLLM,
	} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("route %q missing from kernel specs", want)
		}
	}
	var haveRedirect bool
	for _, s := range specs {
		if s.name == kernelRouteHTTPRedirect {
			haveRedirect = true
			if s.sectionName != httpRedirectListenerName {
				t.Fatalf("redirect route sectionName = %q, want %q", s.sectionName, httpRedirectListenerName)
			}
			if s.host != "" {
				t.Fatalf("redirect route host = %q, want empty (match all hosts)", s.host)
			}
		}
	}
	if !haveRedirect {
		t.Fatalf("route %q missing from kernel specs", kernelRouteHTTPRedirect)
	}
	llmRoute := buildKernelHTTPRoute(specs[len(specs)-1])
	if llmRoute.Name != kernelRouteLiteLLM {
		t.Fatalf("llm route name = %q, want %q", llmRoute.Name, kernelRouteLiteLLM)
	}
	if string(llmRoute.Spec.Hostnames[0]) != "llm.platform.example.test" {
		t.Fatalf("llm host = %v", llmRoute.Spec.Hostnames[0])
	}
	backend := llmRoute.Spec.Rules[0].BackendRefs[0]
	if string(backend.Name) != litellmProxyServiceName {
		t.Fatalf("llm backend service = %q, want %q", backend.Name, litellmProxyServiceName)
	}
	// The LLM namespace, not the services one. This asserted servicesNamespace,
	// which on v5 is the edge -- so the route pointed at a litellm-proxy that
	// was never there, and a route whose backend does not resolve answers 503
	// on a host that looks configured. The test agreed with the bug because it
	// read the same variable the code did.
	if backend.Namespace == nil || string(*backend.Namespace) != llmNamespace {
		t.Fatalf("llm backend namespace = %v, want %s", backend.Namespace, llmNamespace)
	}
	if llmNamespace == servicesNamespace {
		t.Fatalf("llm and the edge share a namespace (%s); the assertion above proves nothing", llmNamespace)
	}
	if got := *backend.Port; got != gatewayv1.PortNumber(litellmProxyPort) {
		t.Fatalf("llm backend port = %d, want %d", got, litellmProxyPort)
	}
}

func TestConsoleRedirectRule(t *testing.T) {
	t.Parallel()
	rule := consoleRedirectRule("console.platform.example.test")
	if len(rule.Filters) != 1 || rule.Filters[0].RequestRedirect == nil {
		t.Fatalf("rule = %+v", rule)
	}
	redirect := rule.Filters[0].RequestRedirect
	if redirect.Hostname == nil || string(*redirect.Hostname) != "console.platform.example.test" {
		t.Fatalf("hostname = %v", redirect.Hostname)
	}
	// Path and query travel: an alias by redirect loses nothing of the request.
	if redirect.Path != nil {
		t.Fatalf("console redirect rewrites the path: %+v", redirect.Path)
	}
}

// TestKernelHTTPRouteSpecsAllBindToAListener guards the HTTP->HTTPS redirect.
//
// buildGateway gives every Gateway a hostname-less :80 listener carrying the
// redirect. A route that omits sectionName attaches to EVERY listener whose
// hostname matches, and a hostname-less listener matches everything — so each
// content route silently attached to :80 as well. Gateway API then ranks a
// route's specific hostname above the redirect route's absent one, so plaintext
// requests were answered with content instead of a redirect and every kernel
// host was reachable unencrypted (verified live: http://<kernel host> -> 200).
//
// The invariant is therefore: every kernel route names exactly one listener.
func TestKernelHTTPRouteSpecsAllBindToAListener(t *testing.T) {
	specs := kernelHTTPRouteSpecs(
		"platform.example.test",
		[]string{"demo.platform.example.test"},
		nil,
		[]string{"demo"},
		true,
		"c1", true, true)
	if len(specs) == 0 {
		t.Fatal("no kernel route specs produced")
	}
	for _, s := range specs {
		if s.sectionName == "" {
			t.Errorf("route %q has no sectionName: it will also attach to the :80 "+
				"redirect listener and be served in plaintext", s.name)
		}
	}
}

// TestKernelHTTPRedirectBindsOnlyToPort80 pins the other half of the invariant:
// the catch-all redirect must stay on :80. If it ever attached to a :443
// listener it would redirect https traffic back to itself, forever.
func TestKernelHTTPRedirectBindsOnlyToPort80(t *testing.T) {
	specs := kernelHTTPRouteSpecs("platform.example.test", nil, nil, nil, false, "c1", true, true)
	var found bool
	for _, s := range specs {
		if s.name != kernelRouteHTTPRedirect {
			continue
		}
		found = true
		if s.sectionName != httpRedirectListenerName {
			t.Fatalf("redirect route bound to %q, want %q", s.sectionName, httpRedirectListenerName)
		}
		if s.host != "" {
			t.Fatalf("redirect route host = %q, want empty so it matches every host", s.host)
		}
	}
	if !found {
		t.Fatalf("route %q missing", kernelRouteHTTPRedirect)
	}
}

// Tenant app routes bind to the kernel Gateway pinned to the tenant listener.
// Without the pin they would also attach to the hostname-less :80 listener and
// outrank the redirect route there, serving the app in the clear.
func TestTenantAppRouteBindsToTenantListener(t *testing.T) {
	refs := tenantGatewayParentRefs(tenantGatewayListenerName("demo"))
	if len(refs) != 1 {
		t.Fatalf("want exactly the kernel Gateway parentRef, got %d", len(refs))
	}
	var kernelRef *gatewayv1.ParentReference
	for i := range refs {
		if string(refs[i].Name) == AuthenticatedGatewayName {
			kernelRef = &refs[i]
		}
	}
	if kernelRef == nil {
		t.Fatal("no kernel Gateway parentRef")
	}
	if kernelRef.SectionName == nil {
		t.Fatal("kernel parentRef has no sectionName; route would also attach to :80")
	}
	if string(*kernelRef.SectionName) != "https-tenant-demo-wildcard" {
		t.Fatalf("sectionName = %q", *kernelRef.SectionName)
	}
}

// A tile that opens a blank window is worse than no tile, and that is what a
// console's own framing defence does to the desktop: the request succeeds, the
// browser refuses to paint it, and nothing anywhere says why.
func TestKernelConsolesMayBeFramedByTheDesktop(t *testing.T) {
	// Not parallel: the observability namespace is resolved once at process
	// start, and the cluster view's route exists only where the layout has
	// one. Setting it here is what makes this test about framing rather than
	// about which layout the test binary happened to start in.
	saved := observabilityNamespace
	observabilityNamespace = "kernel-observability"
	t.Cleanup(func() { observabilityNamespace = saved })

	specs := kernelHTTPRouteSpecs("platform.example.test", nil, nil, nil, false, "c1", true, true)
	for _, name := range []string{kernelRouteArgoCD, kernelRouteHeadlamp} {
		var spec *kernelHTTPRouteSpec
		for i := range specs {
			if specs[i].name == name {
				spec = &specs[i]
			}
		}
		if spec == nil {
			t.Fatalf("route %q missing", name)
		}
		f := spec.rules[0].Filters
		if len(f) != 1 || f[0].ResponseHeaderModifier == nil {
			t.Fatalf("%s: no response header filter", name)
		}
		m := f[0].ResponseHeaderModifier
		// X-Frame-Options cannot express an exception, so it goes; the policy
		// that can names the kernel domain and nothing wider.
		if len(m.Remove) != 1 || m.Remove[0] != "X-Frame-Options" {
			t.Errorf("%s: removes %v, want X-Frame-Options", name, m.Remove)
		}
		if len(m.Set) != 1 || m.Set[0].Value != "frame-ancestors 'self' https://*.platform.example.test" {
			t.Errorf("%s: sets %v", name, m.Set)
		}
	}
}

// Keycloak's administration console calls its own Admin REST API with a token
// its own code flow minted inside the page. The edge must leave that header
// alone -- neither strip it, which answers 401 and leaves the console on its
// spinner, nor replace it with the edge's own, which answers "Token issued for
// an application that is not the admin console".
func TestTheKeycloakConsoleKeepsItsOwnBearer(t *testing.T) {
	t.Parallel()
	specs := kernelHTTPRouteSpecs("platform.example.test", nil, nil, nil, false, "c1", true, true)
	for _, s := range specs {
		if s.name != kernelRouteKeycloakAdmin {
			continue
		}
		if s.authz == nil || !s.authz.keepClientToken {
			t.Fatalf("the console's own bearer must survive the edge: %+v", s.authz)
		}
		if s.authz.forwardToken {
			t.Fatalf("the edge must not put its own token on this route: %+v", s.authz)
		}
		// And the two must reach their two destinations: the table line the
		// authorization service reads, and the policy Envoy reads.
		table, err := bouncerRouteTable([]kernelHTTPRouteSpec{s}, nil, "platform.example.test", "kernel")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(table, "keepClientToken: true") || strings.Contains(table, "forwardToken") {
			t.Fatalf("the table line the authorization service reads:\n%s", table)
		}
		return
	}
	t.Fatal("the admin console route is missing")
}

// Under TENANCY_MODE=single the one tenant is on the kernel domain, which the
// kernel's catch-all listener already serves. A *.<kernel> tenant listener
// beside it would be the more specific match for argocd.<kernel> and every
// other kernel host, and route them nowhere.
func TestASingleTenantClusterAddsNoTenantListener(t *testing.T) {
	t.Parallel()
	tenants := []gentianov1alpha1.Tenant{{ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.SingleTenantName}}}
	gw := buildAuthenticatedGateway("example.org", gentianov1alpha1.TenancyModeSingle, tenants)
	for _, l := range gw.Spec.Listeners {
		if l.Hostname != nil && string(*l.Hostname) == "*.example.org" && string(l.Name) != wildcardListenerName {
			t.Fatalf("listener %s claims the kernel domain", l.Name)
		}
		if string(l.Name) == tenantGatewayListenerName(gentianov1alpha1.SingleTenantName) {
			t.Fatalf("the single tenant got a listener of its own: %+v", l)
		}
	}
}

// The cluster's bare domain is not a kernel route: it is a perimeter surface
// of the platform tenant, published from that tenant's DMZ. Nothing here
// routes it, the identity provider's route carries nothing but the identity
// provider, and www is sent to the bare domain.
func TestTheBareDomainIsNotAKernelRoute(t *testing.T) {
	t.Parallel()
	var www string
	for _, spec := range kernelHTTPRouteSpecs("k.example", nil, nil, nil, false, "c1", true, true) {
		if spec.host == "k.example" {
			t.Fatalf("route %q serves the bare domain from the kernel", spec.name)
		}
		switch spec.name {
		case kernelRouteWWWRedirect:
			www = string(*spec.rules[0].Filters[0].RequestRedirect.Hostname)
		case kernelRouteKeycloakIDP:
			for _, rule := range spec.rules {
				if v := rule.Matches[0].Path.Value; v == nil || !strings.HasPrefix(*v, "/auth/") {
					t.Fatalf("the identity provider's public route carries a path that is not its own: %v", v)
				}
			}
		}
	}
	if www != "k.example" {
		t.Fatalf("www -> %q, want the bare domain", www)
	}
}

// The tenant that adopts the kernel realm and has no catalogue apps gets no
// listener: its components are in the kernel zone. Every other tenant does,
// apps or not, because its desktop and consoles are served on it.
func TestOnlyTenantsWithAZoneGetAListener(t *testing.T) {
	t.Parallel()
	platform := *platformTenantFixture()
	acme := *acmeTenantFixture()
	gw := buildAuthenticatedGateway("k.example", "multi", zonedTenants([]gentianov1alpha1.Tenant{platform, acme}, "kernel"))
	names := map[string]bool{}
	for _, l := range gw.Spec.Listeners {
		names[string(l.Name)] = true
	}
	if names[tenantGatewayListenerName("platform")] {
		t.Fatal("the platform tenant got a listener nothing is served on")
	}
	if !names[tenantGatewayListenerName("acme")] {
		t.Fatal("a tenant with no catalogue apps lost its listener")
	}
}

// A tenant with a zone of its own has one whether or not it installs
// catalogue apps; the tenant in the kernel zone and one on the kernel domain
// do not.
func TestATenantWithoutAppsStillHasItsZone(t *testing.T) {
	t.Parallel()
	r := &TenantReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	if !r.tenantHasOwnZone(acmeTenantFixture()) {
		t.Fatal("a tenant with no apps has no zone")
	}
	if r.tenantHasOwnZone(platformTenantFixture()) {
		t.Fatal("the kernel realm's tenant has a zone of its own")
	}
	single := &TenantReconciler{KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "single"}
	if single.tenantHasOwnZone(acmeTenantFixture()) {
		t.Fatal("a tenant on the kernel domain has a zone of its own")
	}
}

// A tenant with no catalogue apps still gets what its listener needs: the
// wildcard certificate and the grants that let the Gateway use it. They were
// composed only beside app routes, so a tenant with a desktop and no apps
// had an invalid listener and a console that answered 502.
func TestATenantWithoutAppsGetsItsCertificateAndGrants(t *testing.T) {
	s := componentDatabaseScheme(t)
	r := &TenantReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s,
		KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi", RoutingMode: RoutingModeGateway,
	}
	objects, err := r.buildTenantEdgeObjects(context.Background(), acmeTenantFixture())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, o := range objects {
		kinds[o.GetObjectKind().GroupVersionKind().Kind]++
		if o.GetObjectKind().GroupVersionKind().Kind == "Certificate" {
			u := o.(*unstructured.Unstructured)
			names, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "dnsNames")
			if len(names) != 1 || names[0] != "*.acme.k.example" {
				t.Fatalf("certificate names = %v", names)
			}
		}
	}
	if kinds["Certificate"] != 1 || kinds["ReferenceGrant"] < 2 || kinds["HTTPRoute"] != 0 {
		t.Fatalf("composed %v", kinds)
	}
	// The kernel realm's tenant has no zone of its own, so nothing is composed.
	if objects, err := r.buildTenantEdgeObjects(context.Background(), platformTenantFixture()); err != nil || len(objects) != 0 {
		t.Fatalf("platform: %d objects, %v", len(objects), err)
	}
}

// Nothing with a backend is routed on the authenticated Gateway without a
// question, and nothing is public on the perimeter that is not on the list
// below. A route on the authenticated Gateway that carries no authz gets no
// session policy at all, so a console added without one answers anyone who
// knows its hostname -- which is how the LiteLLM console stood, and what this
// refuses for whatever is added next. Adding to the perimeter's list is a
// decision to publish something to the internet, and has to be made here, in
// a line somebody reviews.
func TestEveryKernelRouteIsBehindAQuestionOrDeliberatelyPublic(t *testing.T) {
	t.Parallel()
	public := map[string]bool{
		kernelRouteKeycloakIDP:  true, // the identity provider's realm endpoints and theme assets
		kernelRouteHTTPRedirect: true, // :80 to :443
	}
	for _, zoneReady := range []bool{true, false} {
		{
			specs := kernelHTTPRouteSpecs("k.example", []string{"demo.k.example"}, nil, []string{"demo"}, true, "c1", zoneReady, true)
			for _, s := range specs {
				backends, redirectOnly := 0, true
				for _, rule := range s.rules {
					backends += len(rule.BackendRefs)
					isRedirect := false
					for _, f := range rule.Filters {
						if f.Type == gatewayv1.HTTPRouteFilterRequestRedirect {
							isRedirect = true
						}
					}
					if !isRedirect || len(rule.BackendRefs) > 0 {
						redirectOnly = false
					}
				}
				switch {
				case redirectOnly && backends == 0:
					// Sends the browser elsewhere and serves nothing.
				case s.authz != nil:
					// Behind the kernel session and an L2 question.
				case s.securityPolicy != nil:
					if action, _ := s.securityPolicy["authorization"].(map[string]interface{})["defaultAction"].(string); action != "Deny" {
						t.Errorf("%s: a policy of its own that is not a refusal", s.name)
					}
				case s.gateway == PerimeterGatewayName && public[s.name]:
					// Public by decision, named above.
				default:
					t.Errorf("%s (%s) serves a backend with no session, no question and no refusal (zoneReady=%v)", s.name, s.host, zoneReady)
				}
				if s.gateway != PerimeterGatewayName && s.authz == nil && backends > 0 {
					t.Errorf("%s (%s) is on the authenticated Gateway with a backend and no authz", s.name, s.host)
				}
			}
		}
	}
}
