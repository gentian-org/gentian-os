package controller

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
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
	if len(gw.Spec.Listeners) != 5 {
		t.Fatalf("listeners = %d, want 5", len(gw.Spec.Listeners))
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
	if gw.Spec.Listeners[2].Name != kernelCollaboraListenerName {
		t.Fatalf("office listener name = %q", gw.Spec.Listeners[2].Name)
	}
	if gw.Spec.Listeners[2].Hostname == nil || string(*gw.Spec.Listeners[2].Hostname) != "office.desk.gentian.org" {
		t.Fatalf("office listener hostname = %v", gw.Spec.Listeners[2].Hostname)
	}
	if gw.Spec.Listeners[3].Name != "https-tenant-demo-wildcard" {
		t.Fatalf("tenant wildcard listener name = %q", gw.Spec.Listeners[3].Name)
	}
	if gw.Spec.Listeners[3].Hostname == nil || string(*gw.Spec.Listeners[3].Hostname) != "*.demo.desk.gentian.org" {
		t.Fatalf("tenant wildcard listener hostname = %v", gw.Spec.Listeners[3].Hostname)
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

func TestTenantIngressSupersededByGateway(t *testing.T) {
	t.Parallel()
	hosts := map[string]struct{}{"matrix.demo.desk.gentian.org": {}}
	ing := &networkingv1.Ingress{
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "matrix.demo.desk.gentian.org"}},
		},
	}
	if !tenantIngressSupersededByGateway(ing, hosts) {
		t.Fatal("expected matrix host ingress to be superseded")
	}
	other := &networkingv1.Ingress{
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "other.demo.desk.gentian.org"}},
		},
	}
	if tenantIngressSupersededByGateway(other, hosts) {
		t.Fatal("unexpected supersession for unrelated host")
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
	route := buildAppHTTPRoute(tenant, "tenant-demo", "element", nil, ingress, "chat.demo.desk.gentian.org", "demo.desk.gentian.org", "desk.gentian.org")
	if route.Name != "httproute-demo-element" {
		t.Fatalf("name = %q", route.Name)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "chat.demo.desk.gentian.org" {
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

func TestBuildAppHTTPRouteOXRootRedirect(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	ingress := &gentianov1alpha1.IngressSpec{
		SubDomain:   "webmail",
		ServiceName: "appsuite",
	}
	oxProfile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ox-appsuite",
			Annotations: map[string]string{
				gentianov1alpha1.AnnotationProfileGatewayRootRedirect: "/appsuite/",
				gentianov1alpha1.AnnotationProfileGatewayAPIBackends:  `[{"pathPrefix":"/appsuite/api","serviceName":"appsuite-api"}]`,
			},
		},
	}
	route := buildAppHTTPRoute(tenant, "tenant-demo", "ox-appsuite", oxProfile, ingress, "webmail.demo.desk.gentian.org", "demo.desk.gentian.org", "desk.gentian.org")
	if len(route.Spec.Rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(route.Spec.Rules))
	}
	redirect := route.Spec.Rules[0].Filters[0].RequestRedirect
	if redirect == nil || redirect.Path == nil || redirect.Path.ReplaceFullPath == nil || *redirect.Path.ReplaceFullPath != "/appsuite/" {
		t.Fatalf("redirect = %+v", redirect)
	}
	if len(route.Spec.Rules[1].BackendRefs) != 1 || string(route.Spec.Rules[1].BackendRefs[0].Name) != "appsuite-api" {
		t.Fatalf("api backend rule = %+v", route.Spec.Rules[1].BackendRefs)
	}
	if len(route.Spec.Rules[2].BackendRefs) != 1 || string(route.Spec.Rules[2].BackendRefs[0].Name) != "appsuite" {
		t.Fatalf("ui backend rule = %+v", route.Spec.Rules[2].BackendRefs)
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
	if !strings.Contains(policy.Origins, "https://files.desk.gentian.org") {
		t.Fatalf("sandbox origins must include kernel Files host, got %q", policy.Origins)
	}
}

func TestBackendTrafficPolicySpecFromIngressAnnotations(t *testing.T) {
	t.Parallel()
	spec := backendTrafficPolicySpecFromIngressAnnotations(map[string]string{
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "3600",
		"nginx.ingress.kubernetes.io/proxy-body-size":    "128m",
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
			"nginx.ingress.kubernetes.io/proxy-read-timeout": "600",
		},
	}
	obj := buildAppBackendTrafficPolicyObject(tenant, "tenant-demo", "element", ingress)
	if obj == nil {
		t.Fatal("expected BackendTrafficPolicy object")
	}
	if obj.GetName() != "btp-demo-element" {
		t.Fatalf("name = %q", obj.GetName())
	}
	refs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "targetRefs")
	if len(refs) != 1 {
		t.Fatalf("targetRefs = %v", refs)
	}
	ref, _ := refs[0].(map[string]interface{})
	if ref["name"] != "httproute-demo-element" {
		t.Fatalf("target route = %v", ref["name"])
	}

	if buildAppBackendTrafficPolicyObject(tenant, "tenant-demo", "plain", &gentianov1alpha1.IngressSpec{}) != nil {
		t.Fatal("expected nil for ingress without policy annotations")
	}
}

func TestKernelHTTPRouteSpecs(t *testing.T) {
	t.Parallel()
	specs := kernelHTTPRouteSpecs("desk.gentian.org", []string{"demo.desk.gentian.org"}, nil, []string{"demo"})
	if len(specs) != 17 {
		t.Fatalf("spec count = %d, want 17", len(specs))
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

func TestKernelPortalServerDataRules(t *testing.T) {
	t.Parallel()
	rules := kernelPortalServerDataRules("nubus-dev-portal-server", 80)
	var portalJSON *gatewayv1.HTTPRouteRule
	for i := range rules {
		if rules[i].Matches[0].Path != nil && rules[i].Matches[0].Path.Value != nil &&
			*rules[i].Matches[0].Path.Value == "/univention/portal/portal.json" {
			portalJSON = &rules[i]
			break
		}
	}
	if portalJSON == nil {
		t.Fatal("missing portal.json route")
	}
	if portalJSON.BackendRefs[0].Name != "nubus-dev-portal-server" {
		t.Fatalf("backend = %v", portalJSON.BackendRefs[0].Name)
	}
}

func TestKernelUMCGatewayShellRules(t *testing.T) {
	t.Parallel()
	rules := kernelUMCGatewayShellRules("nubus-dev-umc-gateway", 80)
	if len(rules) != 11 {
		t.Fatalf("rule count = %d, want 11", len(rules))
	}
	if len(rules) > 16 {
		t.Fatal("UMC shell route exceeds HTTPRoute rule limit")
	}
	var management *gatewayv1.HTTPRouteRule
	for i := range rules {
		if rules[i].Matches[0].Path == nil || rules[i].Matches[0].Path.Value == nil {
			continue
		}
		if *rules[i].Matches[0].Path.Value == "/univention/management" {
			management = &rules[i]
			break
		}
	}
	if management == nil {
		t.Fatal("missing /univention/management rule")
	}
}

func TestKernelUMCGatewayRules(t *testing.T) {
	t.Parallel()
	rules := kernelUMCGatewayRules("nubus-dev-umc-gateway", 80)
	if len(rules) != 12 {
		t.Fatalf("rule count = %d, want 12", len(rules))
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
	var univentionRedirect *gatewayv1.HTTPRouteRule
	for _, rule := range kernelPortalFrontendRules("nubus-dev-portal-frontend", 80) {
		if len(rule.Matches) == 0 || rule.Matches[0].Path == nil || rule.Matches[0].Path.Value == nil {
			continue
		}
		if *rule.Matches[0].Path.Value == "/univention/" && len(rule.Filters) > 0 && rule.Filters[0].RequestRedirect != nil {
			univentionRedirect = &rule
			break
		}
	}
	if univentionRedirect == nil {
		t.Fatal("missing /univention/ redirect rule")
	}
	if univentionRedirect.Filters[0].RequestRedirect.Path == nil ||
		univentionRedirect.Filters[0].RequestRedirect.Path.ReplaceFullPath == nil ||
		*univentionRedirect.Filters[0].RequestRedirect.Path.ReplaceFullPath != "/login/" {
		t.Fatalf("/univention/ redirect target = %v", univentionRedirect.Filters[0].RequestRedirect.Path)
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
