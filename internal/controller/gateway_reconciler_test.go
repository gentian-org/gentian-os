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
	if len(gw.Spec.Listeners) != 4 {
		t.Fatalf("listeners = %d, want 4", len(gw.Spec.Listeners))
	}
	if gw.Spec.Listeners[0].Name != "https-wildcard" {
		t.Fatalf("wildcard listener name = %q", gw.Spec.Listeners[0].Name)
	}
	if gw.Spec.Listeners[0].Hostname == nil || string(*gw.Spec.Listeners[0].Hostname) != "*.desk.gentian.org" {
		t.Fatalf("wildcard listener hostname = %v", gw.Spec.Listeners[0].Hostname)
	}
	if gw.Spec.Listeners[1].Name != "https-apex" {
		t.Fatalf("apex listener name = %q", gw.Spec.Listeners[1].Name)
	}
	if gw.Spec.Listeners[1].Hostname == nil || string(*gw.Spec.Listeners[1].Hostname) != "desk.gentian.org" {
		t.Fatalf("apex listener hostname = %v", gw.Spec.Listeners[1].Hostname)
	}
	if gw.Spec.Listeners[2].Name != "https-tenant-demo-wildcard" {
		t.Fatalf("tenant wildcard listener name = %q", gw.Spec.Listeners[2].Name)
	}
	if gw.Spec.Listeners[2].Hostname == nil || string(*gw.Spec.Listeners[2].Hostname) != "*.demo.desk.gentian.org" {
		t.Fatalf("tenant wildcard listener hostname = %v", gw.Spec.Listeners[2].Hostname)
	}
	if gw.Spec.Listeners[0].AllowedRoutes == nil || gw.Spec.Listeners[0].AllowedRoutes.Namespaces.From == nil ||
		*gw.Spec.Listeners[0].AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromAll {
		t.Fatalf("kernel gateway should allow cross-namespace routes, got %v", gw.Spec.Listeners[0].AllowedRoutes)
	}
	if string(gw.Spec.Listeners[0].TLS.CertificateRefs[0].Name) != kernelWildcardTLSSecretName {
		t.Fatalf("tls secret = %q", gw.Spec.Listeners[0].TLS.CertificateRefs[0].Name)
	}
}

func TestBuildTenantGateway(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	gw := buildTenantGateway(tenant, "tenant-demo", "demo.desk.gentian.org", "tenant-demo-wildcard-tls")
	if gw.Name != "tenant-demo-gateway" {
		t.Fatalf("name = %q", gw.Name)
	}
	if gw.Namespace != "tenant-demo" {
		t.Fatalf("namespace = %q", gw.Namespace)
	}
	if gw.Labels[tenantLabel] != "demo" {
		t.Fatalf("tenant label = %q", gw.Labels[tenantLabel])
	}
	if string(*gw.Spec.Listeners[0].Hostname) != "*.demo.desk.gentian.org" {
		t.Fatalf("hostname = %v", gw.Spec.Listeners[0].Hostname)
	}
}

func TestGatewayProgrammed(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("install gateway scheme: %v", err)
	}

	gw := buildTenantGateway(
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}},
		"tenant-demo", "demo.desk.gentian.org", "tenant-demo-wildcard-tls",
	)
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

	gw := buildTenantGateway(
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}},
		"tenant-demo", "demo.desk.gentian.org", "tenant-demo-wildcard-tls",
	)
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
	if len(route.Spec.ParentRefs) != 2 {
		t.Fatalf("parent refs = %d, want 2", len(route.Spec.ParentRefs))
	}
	if route.Spec.ParentRefs[0].Name != "tenant-demo-gateway" {
		t.Fatalf("tenant parent = %v", route.Spec.ParentRefs[0].Name)
	}
	if route.Spec.ParentRefs[1].Name != KernelPublicGatewayName {
		t.Fatalf("kernel parent = %v", route.Spec.ParentRefs[1].Name)
	}
	if route.Spec.ParentRefs[1].Namespace == nil || string(*route.Spec.ParentRefs[1].Namespace) != servicesNamespace {
		t.Fatalf("kernel parent namespace = %v", route.Spec.ParentRefs[1].Namespace)
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
	if len(specs) != 5 {
		t.Fatalf("spec count = %d, want 5 with LLM_SUPPORT=true", len(specs))
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
}
