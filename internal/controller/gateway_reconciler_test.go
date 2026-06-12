package controller

import (
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestNormalizeRoutingMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", RoutingModeIngress},
		{"ingress", RoutingModeIngress},
		{"GATEWAY", RoutingModeGateway},
		{" gateway ", RoutingModeGateway},
		{"nginx", RoutingModeIngress},
	}
	for _, tc := range tests {
		if got := normalizeRoutingMode(tc.in); got != tc.want {
			t.Fatalf("normalizeRoutingMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildKernelGateway(t *testing.T) {
	t.Parallel()
	gw := buildKernelGateway("desk.gentian.org")
	if gw.Name != KernelPublicGatewayName {
		t.Fatalf("name = %q", gw.Name)
	}
	if gw.Namespace != servicesNamespace {
		t.Fatalf("namespace = %q", gw.Namespace)
	}
	if string(gw.Spec.GatewayClassName) != GentianGatewayClassName {
		t.Fatalf("gatewayClassName = %q", gw.Spec.GatewayClassName)
	}
	if len(gw.Spec.Listeners) != 2 {
		t.Fatalf("listeners = %d, want 2", len(gw.Spec.Listeners))
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
		SubDomain: "chat",
	}
	route := buildAppHTTPRoute(tenant, "tenant-demo", "element", ingress, "chat.demo.desk.gentian.org", "demo.desk.gentian.org", "desk.gentian.org")
	if route.Name != "httproute-demo-element" {
		t.Fatalf("name = %q", route.Name)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "chat.demo.desk.gentian.org" {
		t.Fatalf("hostnames = %v", route.Spec.Hostnames)
	}
	if route.Spec.ParentRefs[0].Name != "tenant-demo-gateway" {
		t.Fatalf("parent = %v", route.Spec.ParentRefs[0].Name)
	}
	if len(route.Spec.Rules[0].Filters) == 0 {
		t.Fatal("expected embedding response filters")
	}
}

func TestBuildTenantApexRedirectHTTPRoute(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	route := buildTenantApexRedirectHTTPRoute(tenant, "tenant-demo", "demo.desk.gentian.org", "desk.gentian.org")
	if route.Name != tenantPortalRedirectName("demo") {
		t.Fatalf("name = %q", route.Name)
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].Filters) != 1 {
		t.Fatalf("rules = %+v", route.Spec.Rules)
	}
	redirect := route.Spec.Rules[0].Filters[0].RequestRedirect
	if redirect == nil || redirect.Scheme == nil || *redirect.Scheme != "https" {
		t.Fatalf("redirect scheme = %v", redirect)
	}
	if redirect.Hostname == nil || string(*redirect.Hostname) != "portal.desk.gentian.org" {
		t.Fatalf("redirect hostname = %v", redirect.Hostname)
	}
	if redirect.Path == nil || redirect.Path.ReplaceFullPath == nil || *redirect.Path.ReplaceFullPath != "/login/" {
		t.Fatalf("redirect path = %v", redirect.Path)
	}
}

func TestComputeGatewayFrameAncestorsPolicy(t *testing.T) {
	t.Parallel()
	policy := computeGatewayFrameAncestorsPolicy("desk.gentian.org", "demo.desk.gentian.org", cryptpadSandboxSubDomain)
	if policy.Mode != gatewayFrameAncestorsAppend {
		t.Fatalf("mode = %q", policy.Mode)
	}
	if !strings.Contains(policy.Origins, "https://pad.demo.desk.gentian.org") {
		t.Fatalf("origins = %q", policy.Origins)
	}
}

func TestBackendTrafficPolicySpecFromIngressAnnotations(t *testing.T) {
	t.Parallel()
	spec := backendTrafficPolicySpecFromIngressAnnotations(map[string]string{
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "3600",
		"nginx.ingress.kubernetes.io/proxy-body-size":      "128m",
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

func TestKernelHTTPRouteSpecs(t *testing.T) {
	t.Parallel()
	specs := kernelHTTPRouteSpecs("desk.gentian.org", []string{"demo.desk.gentian.org"}, nil, []string{"demo"})
	if len(specs) != 15 {
		t.Fatalf("spec count = %d, want 15", len(specs))
	}
	idRoute := buildKernelHTTPRoute(specs[0])
	if idRoute.Name != kernelRouteKeycloakIDP {
		t.Fatalf("id route name = %q", idRoute.Name)
	}
	if string(idRoute.Spec.Hostnames[0]) != "id.desk.gentian.org" {
		t.Fatalf("id host = %v", idRoute.Spec.Hostnames[0])
	}
	if got := *idRoute.Spec.Rules[0].BackendRefs[0].Port; got != gatewayv1.PortNumber(keycloakProxyServicePort) {
		t.Fatalf("id backend port = %d, want %d", got, keycloakProxyServicePort)
	}

	var baseRouter, udm *gatewayv1.HTTPRoute
	for i := range specs {
		switch specs[i].name {
		case kernelRoutePortalBaseRouter:
			baseRouter = buildKernelHTTPRoute(specs[i])
		case kernelRoutePortalUDM:
			udm = buildKernelHTTPRoute(specs[i])
		}
	}
	if baseRouter == nil {
		t.Fatal("missing base-router spec")
	}
	if got := *baseRouter.Spec.Rules[0].BackendRefs[0].Port; got != gatewayv1.PortNumber(baseRouterServicePort) {
		t.Fatalf("base-router port = %d, want %d", got, baseRouterServicePort)
	}
	if udm == nil {
		t.Fatal("missing udm spec")
	}
	if got := *udm.Spec.Rules[0].BackendRefs[0].Port; got != gatewayv1.PortNumber(udmRestAPIServicePort) {
		t.Fatalf("udm port = %d, want %d", got, udmRestAPIServicePort)
	}
}

func TestKernelUMCGatewayRules(t *testing.T) {
	t.Parallel()
	rules := kernelUMCGatewayRules("nubus-dev-umc-gateway", 80)
	if len(rules) != 10 {
		t.Fatalf("rule count = %d, want 10", len(rules))
	}
	var oidc *gatewayv1.HTTPRouteRule
	for i := range rules {
		if rules[i].Matches[0].Path == nil || rules[i].Matches[0].Path.Value == nil {
			continue
		}
		if *rules[i].Matches[0].Path.Value == "/univention/oidc" {
			oidc = &rules[i]
			break
		}
	}
	if oidc == nil {
		t.Fatal("missing /univention/oidc rule")
	}
	if oidc.BackendRefs[0].Name != "nubus-dev-umc-gateway" {
		t.Fatalf("backend = %v", oidc.BackendRefs[0].Name)
	}
}

func TestKernelPortalFrontendRewriteRules(t *testing.T) {
	t.Parallel()
	rules := kernelPortalFrontendRewriteRules("nubus-dev-portal-frontend", 80)
	if len(rules) == 0 {
		t.Fatal("expected rewrite rules")
	}
	if len(kernelPortalFrontendAssetRewriteRules("nubus-dev-portal-frontend", 80)) > 16 {
		t.Fatal("asset rewrite route exceeds HTTPRoute rule limit")
	}
	if len(kernelPortalFrontendAppRewriteRules("nubus-dev-portal-frontend", 80)) > 16 {
		t.Fatal("app rewrite route exceeds HTTPRoute rule limit")
	}
	if len(kernelPortalFrontendRules("nubus-dev-portal-frontend", 80)) > 16 {
		t.Fatal("portal route exceeds HTTPRoute rule limit")
	}
	var cssRewrite *gatewayv1.HTTPRouteRule
	for i := range rules {
		if len(rules[i].Matches) == 0 || rules[i].Matches[0].Path == nil || rules[i].Matches[0].Path.Value == nil {
			continue
		}
		if *rules[i].Matches[0].Path.Value == "/univention/portal/css" {
			cssRewrite = &rules[i]
			break
		}
	}
	if cssRewrite == nil {
		t.Fatal("missing /univention/portal/css rewrite rule")
	}
	if len(cssRewrite.Filters) != 1 || cssRewrite.Filters[0].URLRewrite == nil {
		t.Fatalf("css rewrite filters = %+v", cssRewrite.Filters)
	}
	if cssRewrite.Filters[0].URLRewrite.Path == nil || cssRewrite.Filters[0].URLRewrite.Path.ReplacePrefixMatch == nil {
		t.Fatalf("css rewrite path = %+v", cssRewrite.Filters[0].URLRewrite.Path)
	}
	if *cssRewrite.Filters[0].URLRewrite.Path.ReplacePrefixMatch != "/css" {
		t.Fatalf("css replace prefix = %q", *cssRewrite.Filters[0].URLRewrite.Path.ReplacePrefixMatch)
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
