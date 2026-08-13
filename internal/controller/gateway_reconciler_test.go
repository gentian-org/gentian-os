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
	gw := buildKernelGateway("desk.gentian.org", "multi", tenants)
	if gw.Name != KernelPublicGatewayName {
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
	// kernel certificate covers desk.gentian.org and *.desk.gentian.org, so a
	// browser may coalesce portal.desk.gentian.org onto an existing
	// desk.gentian.org connection; with a listener scoped to the apex that
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
	if tenantL.Hostname == nil || string(*tenantL.Hostname) != "*.demo.desk.gentian.org" {
		t.Fatalf("tenant listener hostname = %v", tenantL.Hostname)
	}
	if _, exists := byName["https-tenant-demo-apex"]; exists {
		t.Fatal("https-tenant-demo-apex still present: the tenant apex is covered by the kernel certificate and served by the catch-all")
	}

	// The :80 redirect listener is hostname-less on purpose: it must match every
	// host so any plaintext request can be bounced to https.
	redirect, ok := byName[httpRedirectListenerName]
	if !ok {
		t.Fatalf("listener %q missing; have %v", httpRedirectListenerName, slices.Sorted(maps.Keys(byName)))
	}
	if redirect.Port != 80 {
		t.Fatalf("redirect listener port = %d, want 80", redirect.Port)
	}
	if redirect.Hostname != nil {
		t.Fatalf("redirect listener hostname = %v, want nil (match all hosts)", *redirect.Hostname)
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
		ObjectMeta: metav1.ObjectMeta{Name: KernelPublicGatewayName, Namespace: "platform-kernel"},
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
		ObjectMeta: metav1.ObjectMeta{Name: KernelPublicGatewayName, Namespace: "platform-kernel"},
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

func TestBuildAppHTTPRoute(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	ingress := &gentianov1alpha1.IngressSpec{
		SubDomain: "app",
	}
	route := buildAppHTTPRoute(tenant, "tenant-demo", ingressIntent{
		appProfile: "catalogue-test-app",
		ingress:    ingress,
	}, "demo.desk.gentian.org", "desk.gentian.org")
	if route.Name != "httproute-demo-catalogue-test-app" {
		t.Fatalf("name = %q", route.Name)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "app.demo.desk.gentian.org" {
		t.Fatalf("hostnames = %v", route.Spec.Hostnames)
	}
	// One parent: the kernel Gateway, pinned to this tenant's listener.
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("parent refs = %d, want 1", len(route.Spec.ParentRefs))
	}
	if route.Spec.ParentRefs[0].Name != KernelPublicGatewayName {
		t.Fatalf("kernel parent = %v", route.Spec.ParentRefs[0].Name)
	}
	if route.Spec.ParentRefs[0].Namespace == nil || string(*route.Spec.ParentRefs[0].Namespace) != servicesNamespace {
		t.Fatalf("kernel parent namespace = %v", route.Spec.ParentRefs[0].Namespace)
	}
	if route.Spec.ParentRefs[0].SectionName == nil ||
		string(*route.Spec.ParentRefs[0].SectionName) != tenantGatewayListenerName("demo") {
		t.Fatalf("kernel parent sectionName = %v", route.Spec.ParentRefs[0].SectionName)
	}
	if len(route.Spec.Rules[0].Filters) == 0 {
		t.Fatal("expected embedding response filters")
	}
}

func TestBuildAppHTTPRouteRootRedirect(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	ingress := &gentianov1alpha1.IngressSpec{
		SubDomain:   "app",
		ServiceName: "ui",
	}
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "multi-route-app",
			Annotations: map[string]string{
				gentianov1alpha1.AnnotationProfileGatewayRootRedirect: "/ui/",
				gentianov1alpha1.AnnotationProfileGatewayAPIBackends:  `[{"pathPrefix":"/ui/api","serviceName":"api"}]`,
			},
		},
	}
	route := buildAppHTTPRoute(tenant, "tenant-demo", ingressIntent{
		appProfile: "multi-route-app",
		profile:    profile,
		ingress:    ingress,
	}, "demo.desk.gentian.org", "desk.gentian.org")
	if len(route.Spec.Rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(route.Spec.Rules))
	}
	redirect := route.Spec.Rules[0].Filters[0].RequestRedirect
	if redirect == nil || redirect.Path == nil || redirect.Path.ReplaceFullPath == nil || *redirect.Path.ReplaceFullPath != "/ui/" {
		t.Fatalf("redirect = %+v", redirect)
	}
	if len(route.Spec.Rules[1].BackendRefs) != 1 || string(route.Spec.Rules[1].BackendRefs[0].Name) != "api" {
		t.Fatalf("api backend rule = %+v", route.Spec.Rules[1].BackendRefs)
	}
	if len(route.Spec.Rules[2].BackendRefs) != 1 || string(route.Spec.Rules[2].BackendRefs[0].Name) != "ui" {
		t.Fatalf("ui backend rule = %+v", route.Spec.Rules[2].BackendRefs)
	}
}

func TestComputeGatewayFrameAncestorsPolicy(t *testing.T) {
	t.Parallel()
	policy := computeGatewayFrameAncestorsPolicy("desk.gentian.org", "demo.desk.gentian.org", "app")
	if policy.Mode != gatewayFrameAncestorsReplace {
		t.Fatalf("mode = %q", policy.Mode)
	}
	if policy.Origins != "https://portal.desk.gentian.org https://demo.desk.gentian.org https://*.demo.desk.gentian.org" {
		t.Fatalf("origins = %q", policy.Origins)
	}
}

func TestIngressGatewayFrameAncestorsPolicy(t *testing.T) {
	t.Parallel()
	ingress := &gentianov1alpha1.IngressSpec{
		Annotations: map[string]string{
			gentianov1alpha1.AnnotationIngressGatewayFrameAncestors: `{"mode":"replace","origins":["mainApp","portal"]}`,
		},
	}
	policy, ok, err := ingressFrameAncestorsPolicy("desk.gentian.org", "demo.desk.gentian.org", "cloud", ingress)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected custom policy")
	}
	if policy.Mode != gatewayFrameAncestorsReplace {
		t.Fatalf("mode = %q", policy.Mode)
	}
	if !strings.Contains(policy.Origins, "https://cloud.demo.desk.gentian.org") {
		t.Fatalf("origins = %q", policy.Origins)
	}
	if !strings.Contains(policy.Origins, "https://portal.desk.gentian.org") {
		t.Fatalf("origins = %q", policy.Origins)
	}
	// The portal answers on the tenant apex too, and that is the host a tenant
	// user is normally signed in on. Leaving it out passes every server-side
	// check and still blocks the iframe in the browser, so assert it explicitly.
	if !strings.Contains(policy.Origins, "https://demo.desk.gentian.org") {
		t.Fatalf("origins = %q", policy.Origins)
	}
}

// The "portal" token must resolve to the same hosts the portal is actually
// routed on, so a policy that opts out of the computed default does not silently
// carry a narrower list than the default it replaced.
func TestIngressGatewayFrameAncestorsPortalTokenMatchesRoutedPortalHosts(t *testing.T) {
	t.Parallel()
	ingress := &gentianov1alpha1.IngressSpec{
		Annotations: map[string]string{
			gentianov1alpha1.AnnotationIngressGatewayFrameAncestors: `{"mode":"replace","origins":["portal"]}`,
		},
	}
	policy, ok, err := ingressFrameAncestorsPolicy("desk.gentian.org", "demo.desk.gentian.org", "cloud", ingress)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected custom policy")
	}
	want := strings.Join(portalOrigins("desk.gentian.org", "demo.desk.gentian.org"), " ")
	if policy.Origins != want {
		t.Fatalf("origins = %q, want %q", policy.Origins, want)
	}
}

// mainApp and portal collapse to one origin when the app is the tenant apex;
// a repeated origin in the header is noise, not a second permission.
func TestIngressGatewayFrameAncestorsDeduplicatesOrigins(t *testing.T) {
	t.Parallel()
	ingress := &gentianov1alpha1.IngressSpec{
		Annotations: map[string]string{
			gentianov1alpha1.AnnotationIngressGatewayFrameAncestors: `{"mode":"replace","origins":["mainApp","portal"]}`,
		},
	}
	policy, _, err := ingressFrameAncestorsPolicy("desk.gentian.org", "demo.desk.gentian.org", "@", ingress)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(policy.Origins, "https://demo.desk.gentian.org"); got != 1 {
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
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				gentianov1alpha1.AnnotationProfileGatewayAPIBackends: `[{"pathPrefix":"/portal-bridge","serviceName":"app-portal-bridge","port":8080}]`,
			},
		},
		Spec: gentianov1alpha1.AppProfileSpec{
			Ingress: &gentianov1alpha1.IngressSpec{SubDomain: "projects"},
		},
	}
	ingress := &gentianov1alpha1.IngressSpec{SubDomain: "projects"}
	rules := appAPIBackendRules(profile, 8080, "desk.gentian.org", "demo.desk.gentian.org", ingress)
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
	if !strings.Contains(modifier.Set[0].Value, "https://portal.desk.gentian.org") {
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

func TestBuildAppBackendTrafficPolicyObject(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	ingress := &gentianov1alpha1.IngressSpec{
		Annotations: map[string]string{
			gentianov1alpha1.AnnotationIngressGatewayRequestTimeout: "600",
		},
	}
	obj := buildAppBackendTrafficPolicyObject(tenant, "tenant-demo", "catalogue-test-app", ingress)
	if obj == nil {
		t.Fatal("expected BackendTrafficPolicy object")
	}
	if obj.GetName() != "btp-demo-catalogue-test-app" {
		t.Fatalf("name = %q", obj.GetName())
	}
	refs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "targetRefs")
	if len(refs) != 1 {
		t.Fatalf("targetRefs = %v", refs)
	}
	ref, _ := refs[0].(map[string]interface{})
	if ref["name"] != "httproute-demo-catalogue-test-app" {
		t.Fatalf("target route = %v", ref["name"])
	}

	if buildAppBackendTrafficPolicyObject(tenant, "tenant-demo", "plain", &gentianov1alpha1.IngressSpec{}) != nil {
		t.Fatal("expected nil for ingress without policy annotations")
	}
}

func TestKernelHTTPRouteSpecs(t *testing.T) {
	t.Parallel()
	specs := kernelHTTPRouteSpecs("desk.gentian.org", []string{"demo.desk.gentian.org"}, nil, []string{"demo"})
	// One route per kernel host, plus one per tenant host serving the portal.
	// Asserted by name rather than by count, so adding a route does not fail a
	// test that has nothing to do with it.
	byName := map[string]kernelHTTPRouteSpec{}
	for _, s := range specs {
		byName[s.name] = s
	}
	for _, want := range []string{
		kernelRouteKeycloakIDP, kernelRouteGentianPortal, kernelRouteKernelApex,
		kernelRouteArgoCD, "tenant-demo-portal",
	} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("missing kernel route %q; got %v", want, specs)
		}
	}
	idRoute := buildKernelHTTPRoute(specs[0])
	if idRoute.Name != kernelRouteKeycloakIDP {
		t.Fatalf("id route name = %q", idRoute.Name)
	}
	if string(idRoute.Spec.Hostnames[0]) != "id.desk.gentian.org" {
		t.Fatalf("id host = %v", idRoute.Spec.Hostnames[0])
	}
	if got := *idRoute.Spec.Rules[0].BackendRefs[0].Port; got != gatewayv1.PortNumber(8080) {
		t.Fatalf("id backend port = %d, want 8080 (Suze Keycloak)", got)
	}
	idNS := idRoute.Spec.Rules[0].BackendRefs[0].Namespace
	if idNS == nil || string(*idNS) != kernelNamespace {
		t.Fatalf("id backend namespace = %v, want %s", idNS, kernelNamespace)
	}
	portalRoute := buildKernelHTTPRoute(specs[1])
	if portalRoute.Name != kernelRouteGentianPortal {
		t.Fatalf("portal route name = %q", portalRoute.Name)
	}
	if string(portalRoute.Spec.Hostnames[0]) != "portal.desk.gentian.org" {
		t.Fatalf("portal host = %v", portalRoute.Spec.Hostnames[0])
	}
	ns := portalRoute.Spec.Rules[0].BackendRefs[0].Namespace
	if ns == nil || string(*ns) != kernelNamespace {
		t.Fatalf("portal api backend namespace = %v, want %s", ns, kernelNamespace)
	}
}

func TestKernelHTTPRouteSpecsLLMDisabledByDefault(t *testing.T) {
	specs := kernelHTTPRouteSpecs("desk.gentian.org", []string{"demo.desk.gentian.org"}, nil, []string{"demo"})
	for _, spec := range specs {
		if spec.name == kernelRouteLiteLLM {
			t.Fatalf("kernel-llm route present without LLM_SUPPORT=true")
		}
	}
}

func TestKernelHTTPRouteSpecsLLMEnabled(t *testing.T) {
	t.Setenv("LLM_SUPPORT", "true")
	specs := kernelHTTPRouteSpecs("desk.gentian.org", nil, nil, nil)
	// 6, not 5: the :80 -> :443 redirect route (kernel-http-redirect) is emitted
	// alongside the apex and argocd routes. The LLM route is still appended last,
	// which is what the specs[len-1] lookup below relies on.
	if len(specs) != 6 {
		t.Fatalf("spec count = %d, want 6 with LLM_SUPPORT=true", len(specs))
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
	if string(llmRoute.Spec.Hostnames[0]) != "llm.desk.gentian.org" {
		t.Fatalf("llm host = %v", llmRoute.Spec.Hostnames[0])
	}
	backend := llmRoute.Spec.Rules[0].BackendRefs[0]
	if string(backend.Name) != litellmProxyServiceName {
		t.Fatalf("llm backend service = %q, want %q", backend.Name, litellmProxyServiceName)
	}
	if backend.Namespace == nil || string(*backend.Namespace) != kernelNamespace {
		t.Fatalf("llm backend namespace = %v, want %s", backend.Namespace, kernelNamespace)
	}
	if got := *backend.Port; got != gatewayv1.PortNumber(litellmProxyPort) {
		t.Fatalf("llm backend port = %d, want %d", got, litellmProxyPort)
	}
}

func TestKernelApexRedirectRule(t *testing.T) {
	t.Parallel()
	rule := kernelApexRedirectRule("desk.gentian.org")
	if len(rule.Filters) != 1 || rule.Filters[0].RequestRedirect == nil {
		t.Fatalf("rule = %+v", rule)
	}
	redirect := rule.Filters[0].RequestRedirect
	if redirect.Hostname == nil || string(*redirect.Hostname) != "portal.desk.gentian.org" {
		t.Fatalf("hostname = %v", redirect.Hostname)
	}
	// No trailing slash: the portal router declares "/login" and TanStack Router
	// does not normalise "/login/", so the apex redirect landed users on the
	// app's not-found page. The static server answers both paths with 200 and
	// index.html, so nothing outside the browser could see it.
	if redirect.Path == nil || redirect.Path.ReplaceFullPath == nil {
		t.Fatalf("apex redirect has no path modifier: %+v", redirect)
	}
	if got := *redirect.Path.ReplaceFullPath; got != "/login" {
		t.Fatalf("apex redirect path = %q, want %q", got, "/login")
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
// host was reachable unencrypted (verified live: http://portal.<domain> -> 200).
//
// The invariant is therefore: every kernel route names exactly one listener.
func TestKernelHTTPRouteSpecsAllBindToAListener(t *testing.T) {
	t.Setenv("LLM_SUPPORT", "true")
	specs := kernelHTTPRouteSpecs(
		"desk.gentian.org",
		[]string{"demo.desk.gentian.org"},
		nil,
		[]string{"demo"},
	)
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
	specs := kernelHTTPRouteSpecs("desk.gentian.org", nil, nil, nil)
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
		if string(refs[i].Name) == KernelPublicGatewayName {
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
