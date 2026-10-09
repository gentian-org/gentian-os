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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/modelgateway"
)

const (
	kernelRouteKeycloakIDP   = "kernel-idp"
	kernelRouteKeycloakAdmin = "kernel-id-admin"
	// kernelRouteKeycloakRefused carries the paths id.<kernel> refuses -- the
	// master realm and the admin console -- as a route of its own, more
	// specific than the allow and closed by a policy: what a perimeter
	// surface refuses is written down, not left to absence.
	kernelRouteKeycloakRefused = "kernel-idp-refused"
	kernelRouteHTTPRedirect    = "kernel-http-redirect"
	kernelRouteArgoCD          = "kernel-argocd"
	kernelRouteHeadlamp        = "kernel-headlamp"
	// The name people type: an alias of the bare domain, by redirect.
	kernelRouteWWWRedirect = "kernel-www-redirect"
	// desktop.<kernel> on a multi-tenancy cluster: nobody's desktop there,
	// and sent to the bare domain like www.
	kernelRouteDesktopRedirect = "kernel-desktop-redirect"
	// The bare domain on a single-tenancy cluster: its front page is sent to
	// the user tenant's desktop. Two routes, for the two listeners the bare
	// domain can be on: the perimeter's exact one while the concierge is
	// published there, and the catch-all when nothing is.
	kernelRouteApexRedirect          = "kernel-apex-redirect"
	kernelRouteApexPerimeterRedirect = "kernel-apex-perimeter-redirect"
	kernelRouteLiteLLM               = "kernel-llm"

	// desktopSubdomain is the desktop's host label in a tenant's zone
	// (networking.md §3): desktop.<t>.<kernel> for a tenant, desktop.<kernel>
	// for the user tenant of a single-tenancy cluster. The desktop profile
	// exposes it under this name, and every redirect and frame policy here
	// assumes it. The platform tenant's desktop is the one exception: the
	// same entry answers on the zone's own name, platform.<kernel>
	// (exposureHostIn).
	desktopSubdomain = addresses.DesktopLabel

	argocdServerServiceName = "argocd-server"
	headlampServiceName     = "headlamp"
	litellmProxyServiceName = modelgateway.ServiceName
	litellmProxyPort        = modelgateway.Port
)

type kernelHTTPRouteSpec struct {
	name string
	// host empty means "match every hostname" — used by the :80 redirect, which
	// must catch apex, wildcard and tenant domains without enumerating them.
	host  string
	rules []gatewayv1.HTTPRouteRule
	// sectionName binds the route to a single Gateway listener. Without it a
	// route attaches to every listener whose hostname matches, which for the
	// HTTP->HTTPS redirect would include the :443 listeners and send TLS
	// requests into an infinite redirect back to themselves.
	sectionName string
	// gateway names the edge the route attaches to; empty is the
	// authenticated Gateway. Only the identity provider's realm endpoints
	// and the :80 redirect are the perimeter's.
	gateway      string
	policy       map[string]interface{}
	clientPolicy map[string]interface{}
	// securityPolicy is a SecurityPolicy of the route's own, for a route with
	// no zone session: the refusal on the identity provider's perimeter.
	securityPolicy map[string]interface{}
	// authz is the L2 question for a route behind the kernel zone's session;
	// nil for a route with no session (the perimeter's) or none yet.
	authz *routeAuthz
}

// suzeKeycloakHTTPServiceName is the keycloakx chart HTTP Service for Stage 1 Suze IdP.
func suzeKeycloakHTTPServiceName() string {
	if v := envOrDefault("KEYCLOAK_HTTP_SERVICE", ""); v != "" {
		return v
	}
	return "gentian-idp-keycloak-keycloakx-http"
}

func (r *GatewayPlatformReconciler) reconcileKernelHTTPRoutes(ctx context.Context) error {
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for kernel HTTPRoutes: %w", err)
	}

	effectiveDomains, tenantNames := zonedTenantDomains(tenantList.Items, r.KernelDomain, r.TenancyMode)
	oidcSubs, err := collectOIDCIngressSubdomainsByTenant(ctx, r.Client, tenantList.Items)
	if err != nil {
		return fmt.Errorf("collect OIDC ingress subdomains: %w", err)
	}
	// A route lives beside the Gateway; the Services it points at live where
	// their own function does, so each of those namespaces grants the
	// reference. Duplicates in the v4 layout, where they are one namespace.
	for _, ns := range dedupe(argocdNamespace, identityNamespace, servicesNamespace, observabilityNamespace) {
		if err := r.ensureRouteReferenceGrant(ctx, ns); err != nil {
			return fmt.Errorf("ensure ReferenceGrant in %s: %w", ns, err)
		}
	}

	door := kernelFrontDoorOf(tenantList.Items, r.KernelDomain, r.kernelRealm(), r.TenancyMode)
	if door.userDesktop != "" {
		if door.website, err = r.mainAddressWebsiteServing(ctx, tenantList.Items); err != nil {
			return fmt.Errorf("main address: %w", err)
		}
	}
	specs := kernelHTTPRouteSpecs(r.KernelDomain, effectiveDomains, oidcSubs, tenantNames,
		clusterLLMEnabled(ctx, r.Client), r.Cluster, r.kernelZoneReady(ctx), desktopPresent(ctx, r.Client),
		door)
	// The identity provider's public route is held to a rate of sign-in
	// posts per client address, and which address that is has to be asked of
	// the cluster (edgeClientAddressHeader), so it is set here and not where
	// the routes are listed.
	clientAddressHeader := edgeClientAddressHeader(ctx, r.Client)
	for i := range specs {
		if specs[i].name == kernelRouteKeycloakIDP {
			specs[i].policy = keycloakRealmBackendTrafficPolicySpec(clientAddressHeader)
		}
	}
	// The bouncer's table first: a route whose policy asks the bouncer before the
	// bouncer knows the host is refused, which is the right direction, but a
	// short one.
	if err := r.ensureBouncerExchangeSecrets(ctx); err != nil {
		return err
	}
	if err := r.ensureBouncerRouteTable(ctx, specs); err != nil {
		return fmt.Errorf("ensure bouncer route table: %w", err)
	}
	expected := make(map[string]struct{}, len(specs))
	expectedPolicies := map[string]struct{}{}
	for _, spec := range specs {
		expected[spec.name] = struct{}{}
		route := buildKernelHTTPRoute(spec)
		if err := ensureHTTPRouteResource(ctx, r.Client, route); err != nil {
			return fmt.Errorf("ensure kernel HTTPRoute %s: %w", spec.name, err)
		}
		if spec.authz != nil || spec.securityPolicy != nil {
			expectedPolicies[kernelSecurityPolicyName(spec.name)] = struct{}{}
			if err := r.ensureKernelSecurityPolicy(ctx, spec); err != nil {
				return fmt.Errorf("ensure kernel SecurityPolicy %s: %w", spec.name, err)
			}
		}
		if spec.policy != nil {
			if err := r.ensureKernelBackendTrafficPolicy(ctx, spec); err != nil {
				return fmt.Errorf("ensure kernel BackendTrafficPolicy %s: %w", spec.name, err)
			}
		}
		if spec.clientPolicy != nil {
			if err := r.ensureKernelClientTrafficPolicy(ctx, spec); err != nil {
				return fmt.Errorf("ensure kernel ClientTrafficPolicy %s: %w", spec.name, err)
			}
		}
	}
	needsWildcard, err := clusterNeedsEscapedSlashesKeepUnchanged(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("detect escaped-slashes gateway policy need: %w", err)
	}
	if needsWildcard {
		if err := r.ensureKernelClientTrafficPolicyNamed(ctx, "kernel-wildcard-escaped-slashes", wildcardListenerName, escapedSlashesKeepUnchangedClientTrafficPolicySpec()); err != nil {
			return fmt.Errorf("ensure kernel wildcard escaped-slashes ClientTrafficPolicy: %w", err)
		}
		for _, tenantName := range tenantNames {
			name := fmt.Sprintf("tenant-%s-wildcard-escaped-slashes", tenantName)
			sectionName := tenantGatewayListenerName(tenantName)
			if err := r.ensureKernelClientTrafficPolicyNamed(ctx, name, sectionName, escapedSlashesKeepUnchangedClientTrafficPolicySpec()); err != nil {
				return fmt.Errorf("ensure kernel tenant wildcard escaped-slashes ClientTrafficPolicy: %w", err)
			}
		}
	}
	if err := r.deleteStaleKernelSecurityPolicies(ctx, expectedPolicies); err != nil {
		return fmt.Errorf("delete stale kernel SecurityPolicies: %w", err)
	}
	return r.deleteStaleKernelHTTPRoutes(ctx, expected)
}

// mainAddressWebsiteServing reports whether the user tenant's website holds
// the cluster's main address and can answer on it: mainAddressHolder names
// it, and its publishing proxy in the tenant's DMZ has an available pod.
func (r *GatewayPlatformReconciler) mainAddressWebsiteServing(ctx context.Context, tenants []gentianov1alpha1.Tenant) (bool, error) {
	profileList := &gentianov1alpha1.ComponentProfileList{}
	if err := r.List(ctx, profileList); err != nil {
		return false, err
	}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{}
	for i := range profileList.Items {
		profiles[profileList.Items[i].Name] = &profileList.Items[i]
	}
	holder := mainAddressHolder(mainAddressInputs{
		Tenants: tenants, Profiles: profiles,
		KernelDomain: r.KernelDomain, KernelRealm: r.kernelRealm(), TenancyMode: r.TenancyMode,
		Now: time.Now(),
	})
	if holder == nil {
		return false, nil
	}
	namespace, name := mainAddressProxyName(holder)
	proxy := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, proxy); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return proxy.Status.AvailableReplicas > 0, nil
}

// desktopPresent reports whether this cluster ships a desktop at all: the
// profile the operator chart installs (ui-restructure.md §1). Without it
// there is no desktop anywhere, and nothing is redirected to one.
func desktopPresent(ctx context.Context, c client.Reader) bool {
	profile := &gentianov1alpha1.ComponentProfile{}
	return c.Get(ctx, client.ObjectKey{Name: DesktopProfileName}, profile) == nil
}

// desktopHost is where a tenant's desktop answers, desktop.<zone>
// (networking.md §3): the name the desktop profile exposes and the
// component reconciler routes. Not the platform tenant's, which is
// platformDesktopHost.
func desktopHost(zoneDomain string) string {
	return desktopSubdomain + "." + zoneDomain
}

// zonedTenantDomains are the tenants with a domain of their own -- one that
// is not the cluster's -- and those domains, index for index.
//
// A tenant on the cluster's domain itself, the user tenant of a
// single-tenancy cluster, has the kernel's listener and certificate, so it
// adds no apex route, no listener policy and no frame origin of its own.
func zonedTenantDomains(tenants []gentianov1alpha1.Tenant, kernelDomain, tenancyMode string) (domains, names []string) {
	for i := range tenants {
		if tenants[i].DeletionTimestamp != nil {
			continue
		}
		if d := tenants[i].EffectiveDomain(kernelDomain, tenancyMode); d != "" && !servedByKernelEdge(d, kernelDomain) {
			domains = append(domains, d)
			names = append(names, tenants[i].Name)
		}
	}
	return domains, names
}

// kernelFrontDoor is what the cluster's own first addresses do, which is the
// one thing about the kernel's routes that differs by tenancy mode.
//
//	multi   <kernel>          the concierge's address form (the platform
//	                          tenant publishes it; not routed here)
//	        www.<kernel>      -> <kernel>
//	        desktop.<kernel>  -> <kernel>
//	single  <kernel>/         -> the user tenant's desktop
//	        <kernel>/branding  still the concierge's: the cluster's brand,
//	                          which every desktop loads from the bare domain
//	        www.<kernel>      -> the user tenant's desktop
//	        desktop.<kernel>  the user tenant's desktop itself (its component)
//
// And on a single-tenancy cluster whose user tenant has a website on the
// main address (main_address.go), once that website's proxy is up:
//
//	<kernel>/         the website (its component's route)
//	<kernel>/sign-in  -> the user tenant's desktop, still
//	<kernel>/branding  still the concierge's
//	www.<kernel>      -> <kernel>, path kept
type kernelFrontDoor struct {
	single bool
	// userDesktop is the user tenant's desktop on a single-tenancy cluster:
	// desktop.<kernel>, or desktop.<its custom domain> when a TenantDomain
	// binds one. Empty until that tenant is Ready, and the bare domain is the
	// concierge's form until then: before that the desktop does not answer,
	// and a browser sent to a name that is not published yet remembers that
	// it is missing.
	userDesktop string
	// website says the user tenant's website is serving the main address:
	// approved for it, and its publishing proxy has a pod that answers. The
	// front page is then the website's and only /sign-in is sent on. Until
	// the proxy answers the front page still leads to the desktop, so the
	// main address never shows an error while a website is coming up.
	website bool
}

// kernelFrontDoorOf reads the front door off the tenants and the mode.
func kernelFrontDoorOf(tenants []gentianov1alpha1.Tenant, kernelDomain, kernelRealm, tenancyMode string) kernelFrontDoor {
	if gentianov1alpha1.NormalizeTenancyMode(tenancyMode) != gentianov1alpha1.TenancyModeSingle {
		return kernelFrontDoor{}
	}
	door := kernelFrontDoor{single: true}
	for i := range tenants {
		t := &tenants[i]
		if t.DeletionTimestamp != nil || tenantAdoptsKernelRealm(t, kernelRealm) ||
			t.Name != gentianov1alpha1.SingleUserTenantName || t.Status.Phase != gentianov1alpha1.TenantPhaseReady {
			continue
		}
		if domain := t.EffectiveDomain(kernelDomain, tenancyMode); domain != "" {
			door.userDesktop = desktopHost(domain)
		}
	}
	return door
}

func kernelHTTPRouteSpecs(
	kernelDomain string,
	tenantEffectiveDomains []string,
	tenantOIDCSubdomains map[string][]string,
	tenantNames []string,
	llmEnabled bool,
	cluster string,
	kernelZoneReady bool,
	desktop bool,
	door kernelFrontDoor,
) []kernelHTTPRouteSpec {
	idHost := fmt.Sprintf("id.%s", kernelDomain)

	kcService := suzeKeycloakHTTPServiceName()
	kcPort := int32(8080)

	idFilters := keycloakGatewayResponseFilters(kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames)
	specs := []kernelHTTPRouteSpec{
		// The identity provider is a kernel-owned perimeter surface: no
		// session, because it is the issuer, and a path allowlist, because it
		// is public. Realm endpoints and the theme assets they load are the
		// whole of it; the master realm is refused by name, and the
		// administration console is a route of its own on this same hostname
		// behind the kernel session (networking.md §3).
		{
			name:        kernelRouteKeycloakIDP,
			host:        idHost,
			gateway:     PerimeterGatewayName,
			sectionName: perimeterIDListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/realms/", idFilters...),
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/resources/", idFilters...),
			},
			policy: keycloakProxyBackendTrafficPolicySpec(),
		},
		// What id.<kernel> refuses, as a route: these prefixes are more
		// specific than the allow above, so Gateway API ranks them first, and
		// the policy on them denies every caller. A refusal that is an object
		// can be read, listed and tested; an absence cannot.
		{
			name:        kernelRouteKeycloakRefused,
			host:        idHost,
			gateway:     PerimeterGatewayName,
			sectionName: perimeterIDListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/realms/master/"),
				// The activation-link endpoint the platform's Keycloak extension
				// adds to every realm. Only the director and the installer call
				// it, both from inside the cluster; it refuses a caller without
				// manage-users anyway, and the perimeter refuses it as well.
				kernelBackendRuleNS(kcService, identityNamespace, kcPort,
					pathMatch(gatewayv1.PathMatchRegularExpression, `/auth/realms/[^/]+/gentian-activation(/.*)?`)),
			},
			securityPolicy: map[string]interface{}{
				"authorization": map[string]interface{}{"defaultAction": "Deny"},
			},
		},
	}
	clusterObject := "cluster:" + cluster
	if kernelZoneReady {
		// Keycloak's administration, on the SAME hostname that issues the
		// tokens, behind the kernel session.
		//
		// It had a hostname of its own, id-admin.<kernel>, which is the shape
		// every other kernel UI has. Keycloak cannot serve it that way: with
		// KC_HOSTNAME and KC_HOSTNAME_ADMIN different, the console takes a
		// token stamped with the issuer's hostname and calls the Admin REST
		// API on the admin hostname, which refuses it, and the console never
		// finishes loading. Upstream closed that as not planned
		// (keycloak/keycloak#42264), so it is a constraint and not a bug to
		// wait out. One hostname carrying two classes of route, told apart by
		// path, is the only shape that works.
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteKeycloakAdmin,
			host:        idHost,
			gateway:     PerimeterGatewayName,
			sectionName: perimeterIDListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/admin/",
					kernelConsoleFrameFilters(kernelDomain)...),
				// The zone's code flow lands on /oauth2/callback: the OIDC
				// filter answers it, but only on a path the route carries.
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, edgeOAuth2Prefix),
			},
			policy: keycloakProxyBackendTrafficPolicySpec(),
			// The bearer on this route is the console's own. Keep it; do not
			// replace it.
			//
			// Keycloak's administration console runs its own code flow inside
			// the page and calls the Admin REST API with the token that flow
			// produced. Stripping Authorization -- what every other kernel
			// route wants, so a backend gets identity headers instead of a
			// token it has no use for -- answered 401 and left the console on
			// its spinner. Forwarding the EDGE's token instead answered "Token
			// issued for an application that is not the admin console", which
			// is true: it was minted for the zone's client. The console needs
			// neither, only to be left alone.
			authz: &routeAuthz{relation: "can_configure", object: clusterObject, keepClientToken: true},
		})
	}
	// The desktop is the tenant's own component, routed where it runs
	// (tenant-<t>): desktop.<zone> for a tenant, platform.<kernel> for the
	// platform tenant.
	//
	// The cluster's bare domain is the first thing anybody typing the
	// cluster's address meets, before any session, and what it does depends
	// on how many user tenants the cluster is for.
	//
	// Multi-tenancy: it is a perimeter surface -- the concierge, a component
	// of the platform tenant, published from that tenant's DMZ on a listener
	// of the perimeter Gateway (networking.md §1) -- and is not routed here at
	// all. www.<kernel> and desktop.<kernel> are the other names people type,
	// and both are sent to it. desktop.<kernel> is nobody's desktop there: it
	// is a tenant's desktop address with the tenant left out, and it lands
	// on the form that asks who is asking.
	//
	// Single-tenancy: there is one user tenant and so nothing to ask. The
	// bare domain's front page and www are sent to its desktop, once it is
	// Ready, and desktop.<kernel> is that desktop -- routed by the tenant's
	// component, so it is not claimed here. The concierge stays published on
	// the bare domain underneath, because the cluster's brand is served from
	// there (/branding/) to every desktop; only the paths a person lands on,
	// / and the form's /sign-in, are sent on. Those two are more specific than
	// the concierge's whole-host route on the same listener, so they win.
	// With a website on the main address the bare domain is the one name
	// for it: www leads there, path kept, as it does on a multi-tenancy
	// cluster. The website cannot take /sign-in: that redirect is the
	// kernel's and more specific than the website's whole-host route.
	frontDoor := kernelDomain
	bareDomainRule := frontPageRedirectRule
	if door.single && door.userDesktop != "" {
		if door.website {
			bareDomainRule = signInRedirectRule
		} else {
			frontDoor = door.userDesktop
		}
	}
	specs = append(specs, kernelHTTPRouteSpec{
		name:        kernelRouteWWWRedirect,
		host:        "www." + kernelDomain,
		sectionName: wildcardListenerName,
		rules:       []gatewayv1.HTTPRouteRule{hostRedirectRule(frontDoor)},
	})
	switch {
	case !door.single:
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteDesktopRedirect,
			host:        desktopHost(kernelDomain),
			sectionName: wildcardListenerName,
			rules:       []gatewayv1.HTTPRouteRule{hostRedirectRule(kernelDomain)},
		})
	case door.userDesktop != "":
		specs = append(specs,
			kernelHTTPRouteSpec{
				name:        kernelRouteApexPerimeterRedirect,
				host:        kernelDomain,
				gateway:     PerimeterGatewayName,
				sectionName: perimeterListenerName(kernelDomain),
				rules:       []gatewayv1.HTTPRouteRule{bareDomainRule(door.userDesktop)},
			},
			// Where nothing is published on the bare domain, it has no
			// listener on the perimeter and arrives on the catch-all.
			kernelHTTPRouteSpec{
				name:        kernelRouteApexRedirect,
				host:        kernelDomain,
				sectionName: wildcardListenerName,
				rules:       []gatewayv1.HTTPRouteRule{bareDomainRule(door.userDesktop)},
			})
	}
	// A tenant's apex likewise sends the browser to the tenant's own desktop.
	// The apex is published with the tenant either way (it is the tenant's
	// name), so the redirect is what makes it answer. Not the platform
	// tenant's: platform.<kernel> is its desktop, not a name beside it.
	if desktop {
		for i, domain := range tenantEffectiveDomains {
			if i >= len(tenantNames) {
				break
			}
			if domain == platformDesktopHost(kernelDomain) {
				continue
			}
			specs = append(specs, kernelHTTPRouteSpec{
				name:        fmt.Sprintf("tenant-%s-apex", tenantNames[i]),
				host:        domain,
				sectionName: wildcardListenerName,
				rules:       []gatewayv1.HTTPRouteRule{hostRedirectRule(desktopHost(domain))},
			})
		}
	}
	specs = append(specs,
		kernelHTTPRouteSpec{
			// Plaintext :80 -> https, bound to the http-redirect listener only.
			name:        kernelRouteHTTPRedirect,
			gateway:     PerimeterGatewayName,
			sectionName: httpRedirectListenerName,
			rules:       []gatewayv1.HTTPRouteRule{kernelHTTPSRedirectRule()},
		},
	)
	// The kernel UIs: "hidden" means behind a session with a platform role,
	// not an internal hostname, and each tool's own login is the second
	// factor (networking.md §3). No zone, no route: never an open one.
	if kernelZoneReady {
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteArgoCD,
			host:        fmt.Sprintf("argocd.%s", kernelDomain),
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRuleCrossNamespace(argocdServerServiceName, argocdNamespace, 80,
					kernelConsoleFrameFilters(kernelDomain)...),
			},
			authz: &routeAuthz{relation: "can_configure", object: clusterObject},
		})
		// The cluster view, read-only: an auditor's right. A route the
		// operator owns is the one that gets a tunnel hostname and a DNS
		// record; only where the layout has an observability namespace, since
		// a route to a Service that does not exist would still claim the host.
		if observabilityNamespace != "" {
			specs = append(specs, kernelHTTPRouteSpec{
				name:        kernelRouteHeadlamp,
				host:        fmt.Sprintf("headlamp.%s", kernelDomain),
				sectionName: wildcardListenerName,
				rules: []gatewayv1.HTTPRouteRule{
					kernelBackendRuleCrossNamespace(headlampServiceName, observabilityNamespace, 80,
						kernelConsoleFrameFilters(kernelDomain)...),
				},
				authz: &routeAuthz{relation: "can_audit", object: clusterObject},
			})
		}
	}
	// LiteLLM admin console — platform-level only (the claim's llm.enabled).
	// Tenants do not get their own route; app-catalogue "litellm" tiles stay
	// unused until per-tenant access is designed (see docs/design/llms.md).
	//
	// Only once the kernel zone exists, like the other consoles: before it
	// there is no session to put the route behind, and a console that is not
	// routed is the safe way to be early.
	if llmEnabled && kernelZoneReady {
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteLiteLLM,
			host:        fmt.Sprintf("llm.%s", kernelDomain),
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				// The LLM namespace, not the services one. servicesNamespace is
				// the edge on v5, so this route pointed at a litellm-proxy that
				// was never there -- and a route whose backend does not resolve
				// answers 503 on a host that looks configured.
				kernelBackendRulePrefixNS(litellmProxyServiceName, llmNamespace, litellmProxyPort, "/"),
			},
			// Behind the kernel session, and for whoever may configure the
			// cluster, like every other kernel console. It had no question
			// at all: a route on the authenticated Gateway with a backend
			// and no authz gets no session policy, so the console and its
			// API answered anyone who knew the hostname, with only LiteLLM's
			// own key check between the internet and the model gateway.
			authz: &routeAuthz{relation: "can_configure", object: clusterObject},
		})
	}
	return specs
}

// kernelBackendRuleNS routes one match to one Service, optionally cross-namespace.
//
// The prefix and exact variants below were full copies of this, differing in
// which path matcher they called.
func kernelBackendRuleNS(serviceName, namespace string, port int32, match gatewayv1.HTTPRouteMatch, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	ref := gatewayv1.BackendObjectReference{
		Name: gatewayv1.ObjectName(serviceName),
		Port: &p,
	}
	if namespace != "" {
		ns := gatewayv1.Namespace(namespace)
		ref.Namespace = &ns
	}
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{match},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: ref}},
		},
	}
	if len(filters) > 0 {
		rule.Filters = filters
	}
	return rule
}

func kernelBackendRulePrefixNS(serviceName, namespace string, port int32, prefix string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return kernelBackendRuleNS(serviceName, namespace, port, pathPrefixMatch(prefix), filters...)
}

// kernelConsoleFrameFilters lets the platform's desktop open a kernel console
// in a window, and nothing else frame one.
//
// Each console defends itself against being framed, which is right against a
// stranger and wrong here: the tile is how a platform administrator is meant
// to reach it. Argo CD is the strict one -- x-frame-options: sameorigin, which
// it cannot be told to drop, because an empty setting falls back to the
// default.
//
// So the Gateway decides, for every kernel host the same way: the header that
// cannot express an exception is removed, and the one that can names the
// platform's desktop. Not the kernel domain as a whole. That named every host
// under it, which on a cluster whose tenants are under the cluster's domain is
// every tenant's every app, and a page of any of them could then put the
// cluster's administration in a frame under a button of its own.
func kernelConsoleFrameFilters(kernelDomain string) []gatewayv1.HTTPRouteFilter {
	if kernelDomain == "" {
		return nil
	}
	return frameAncestorsFilters([]string{platformDesktopHost(kernelDomain)})
}

// frameAncestorsFilters is the frame policy of a route: the page itself and
// the named hosts may frame it, and nobody else. X-Frame-Options goes because
// it cannot name an exception, and the backend's own Content-Security-Policy
// is replaced because the edge, which knows where the desktop is, is the one
// place that can say who the framer may be.
func frameAncestorsFilters(hosts []string) []gatewayv1.HTTPRouteFilter {
	value := "frame-ancestors 'self'"
	for _, h := range hosts {
		if h != "" {
			value += " https://" + h
		}
	}
	return []gatewayv1.HTTPRouteFilter{{
		Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Remove: []string{"X-Frame-Options"},
			Set:    []gatewayv1.HTTPHeader{{Name: "Content-Security-Policy", Value: value}},
		},
	}}
}

func kernelBackendRuleCrossNamespace(serviceName, namespace string, port int32, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	ns := gatewayv1.Namespace(namespace)
	return gatewayv1.HTTPRouteRule{
		Filters: filters,
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name:      gatewayv1.ObjectName(serviceName),
						Namespace: &ns,
						Port:      &p,
					},
				},
			},
		},
	}
}

// ensureRouteReferenceGrant lets the kernel gateway's HTTPRoutes, which live in
// the services namespace, reference Services in ns.
//
// This was two functions — one for argocd, one for platform-kernel — identical
// for forty lines apart from the namespace they targeted. Which is exactly the
// shape that goes wrong quietly: a change to the grant made in one and not the
// other leaves half the kernel's routes unable to resolve their backend, and the
// symptom is a 500 from the gateway rather than anything naming a ReferenceGrant.
func (r *GatewayPlatformReconciler) ensureRouteReferenceGrant(ctx context.Context, ns string) error {
	spec := map[string]interface{}{
		"from": []interface{}{
			map[string]interface{}{
				"group":     gatewayv1.GroupName,
				"kind":      "HTTPRoute",
				"namespace": servicesNamespace,
			},
		},
		"to": []interface{}{
			map[string]interface{}{
				"group": "",
				"kind":  "Service",
			},
		},
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(referenceGrantGVK)
	desired.SetName("allow-kernel-gateway-routes")
	desired.SetNamespace(ns)
	desired.SetLabels(map[string]string{
		managedByLabel: managedByValue,
	})
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(referenceGrantGVK)
	err := r.Get(ctx, client.ObjectKey{Name: desired.GetName(), Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// kernelHTTPSRedirectRule sends any plaintext request straight to https on the
// same host and path.
//
// 301 (permanent) is the industry-standard status for http->https: it is
// cacheable, and it is what HSTS preload and every scanner expects. Gateway API
// permits only 301 or 302 here, so 308 — which would additionally preserve the
// request method — is not available; that is immaterial in practice because the
// requests reaching :80 are browsers issuing GET on a bare hostname.
//
// Hostname is deliberately not set, so the requested host is preserved and one
// rule covers apex, wildcard and tenant domains.
func kernelHTTPSRedirectRule() gatewayv1.HTTPRouteRule {
	scheme := "https"
	status := 301
	port := gatewayv1.PortNumber(443)
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Scheme:     &scheme,
					Port:       &port,
					StatusCode: &status,
				},
			},
		},
	}
}

// hostRedirectRule sends every request on a host to another host, keeping
// path and query: a redirect that replaced them dropped whatever travelled on
// the request, which is why the portal used to be served on these names
// rather than redirected. Nothing travels on them now -- the session is in
// cookies on the zone's domain -- so an alias by redirect loses nothing.
func hostRedirectRule(target string) gatewayv1.HTTPRouteRule {
	scheme := "https"
	status := 302
	port := gatewayv1.PortNumber(443)
	host := gatewayv1.PreciseHostname(target)
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Scheme:     &scheme,
					Hostname:   &host,
					Port:       &port,
					StatusCode: &status,
				},
			},
		},
	}
}

// frontPageRedirectRule sends the bare domain's front page to a desktop: the
// root, and the concierge's form under /sign-in, each to the desktop's root.
// Nothing else on the host is touched -- the brand under /branding/ is the
// concierge's to serve.
func frontPageRedirectRule(desktop string) gatewayv1.HTTPRouteRule {
	return desktopRedirectRule(desktop,
		pathMatch(gatewayv1.PathMatchExact, "/"),
		pathPrefixMatch(mainAddressSignInPrefix))
}

// signInRedirectRule is what is left of that redirect while a website holds
// the main address: /sign-in alone, which always leads to the desktop and so
// to sign-in, whatever the website does.
func signInRedirectRule(desktop string) gatewayv1.HTTPRouteRule {
	return desktopRedirectRule(desktop, pathPrefixMatch(mainAddressSignInPrefix))
}

func desktopRedirectRule(desktop string, matches ...gatewayv1.HTTPRouteMatch) gatewayv1.HTTPRouteRule {
	scheme := "https"
	status := 302
	port := gatewayv1.PortNumber(443)
	host := gatewayv1.PreciseHostname(desktop)
	root := "/"
	return gatewayv1.HTTPRouteRule{
		Matches: matches,
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Scheme:     &scheme,
				Hostname:   &host,
				Port:       &port,
				StatusCode: &status,
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: &root,
				},
			},
		}},
	}
}

func keycloakProxyBackendTrafficPolicySpec() map[string]interface{} {
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
			},
		},
		"connection": map[string]interface{}{
			"bufferLimit": "128k",
		},
	}
}

// keycloakRealmBackendTrafficPolicySpec is the policy of the identity
// provider's public route: the proxy's, and a limit on how fast one client
// address may post to a sign-in page (edge_rate_limit.go). Pages, their
// assets and the token endpoint are not limited.
func keycloakRealmBackendTrafficPolicySpec(clientAddressHeader string) map[string]interface{} {
	spec := keycloakProxyBackendTrafficPolicySpec()
	if limit := edgeSignInRateLimit(keycloakSignInPostPattern, clientAddressHeader); limit != nil {
		spec["rateLimit"] = limit
	}
	return spec
}

func escapedSlashesKeepUnchangedClientTrafficPolicySpec() map[string]interface{} {
	return map[string]interface{}{
		"path": map[string]interface{}{
			// WOPI/WebSocket URLs may embed encoded paths (%3A, %2F, …).
			// Envoy default normalization rejects them with path_normalization_failed.
			"escapedSlashesAction": "KeepUnchanged",
		},
	}
}

func buildKernelHTTPRoute(spec kernelHTTPRouteSpec) *gatewayv1.HTTPRoute {
	gateway := spec.gateway
	if gateway == "" {
		gateway = AuthenticatedGatewayName
	}
	parentRef := gatewayParentRef(gateway)
	if spec.sectionName != "" {
		s := gatewayv1.SectionName(spec.sectionName)
		parentRef.SectionName = &s
	}

	// An empty Hostnames list matches every host, which is what the :80
	// redirect needs. Emitting []Hostname{""} instead would be rejected.
	var hostnames []gatewayv1.Hostname
	if spec.host != "" {
		hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(spec.host)}
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.name,
			Namespace: servicesNamespace,
			Labels: map[string]string{
				managedByLabel:        managedByValue,
				gatewayComponentLabel: gatewayComponentKernel,
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					parentRef,
				},
			},
			Hostnames: hostnames,
			Rules:     spec.rules,
		},
	}
}

var clientTrafficPolicyGVK = schema.GroupVersionKind{
	Group:   "gateway.envoyproxy.io",
	Version: "v1alpha1",
	Kind:    "ClientTrafficPolicy",
}

func (r *GatewayPlatformReconciler) ensureKernelClientTrafficPolicy(ctx context.Context, spec kernelHTTPRouteSpec) error {
	if spec.clientPolicy == nil {
		return nil
	}
	return r.ensureKernelClientTrafficPolicyNamed(ctx, spec.name, wildcardListenerName, spec.clientPolicy)
}

func (r *GatewayPlatformReconciler) ensureKernelClientTrafficPolicyNamed(
	ctx context.Context,
	policyName, sectionName string,
	clientPolicy map[string]interface{},
) error {
	policySpec := cloneMap(clientPolicy)
	attachKernelClientTrafficPolicyTarget(policySpec, sectionName)

	name := fmt.Sprintf("ctp-%s", policyName)
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(clientTrafficPolicyGVK)
	desired.SetName(name)
	desired.SetNamespace(servicesNamespace)
	desired.SetLabels(map[string]string{
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentKernel,
	})
	if err := unstructured.SetNestedField(desired.Object, policySpec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(clientTrafficPolicyGVK)
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, policySpec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func attachKernelClientTrafficPolicyTarget(spec map[string]interface{}, sectionName string) {
	spec["targetRefs"] = []interface{}{
		map[string]interface{}{
			"group":       gatewayv1.GroupName,
			"kind":        "Gateway",
			"name":        AuthenticatedGatewayName,
			"sectionName": sectionName,
		},
	}
}

func (r *GatewayPlatformReconciler) ensureKernelBackendTrafficPolicy(ctx context.Context, spec kernelHTTPRouteSpec) error {
	if spec.policy == nil {
		return nil
	}
	policySpec := cloneMap(spec.policy)
	attachBackendTrafficPolicyTarget(policySpec, spec.name)

	name := fmt.Sprintf("btp-%s", spec.name)
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(backendTrafficPolicyGVK)
	desired.SetName(name)
	desired.SetNamespace(servicesNamespace)
	desired.SetLabels(map[string]string{
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentKernel,
	})
	if err := unstructured.SetNestedField(desired.Object, policySpec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(backendTrafficPolicyGVK)
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, policySpec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *GatewayPlatformReconciler) deleteStaleKernelHTTPRoutes(ctx context.Context, expected map[string]struct{}) error {
	list := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, list,
		client.InNamespace(servicesNamespace),
		client.MatchingLabels{managedByLabel: managedByValue, gatewayComponentLabel: gatewayComponentKernel},
	); err != nil {
		return err
	}
	for i := range list.Items {
		if _, ok := expected[list.Items[i].Name]; ok {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func kernelKeycloakHTTPRouteName() string {
	return kernelRouteKeycloakIDP
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// dedupe returns the distinct namespaces, in order. Several functions share
// one namespace in the v4 layout and a grant applied twice is a conflict.
func dedupe(ns ...string) []string {
	seen := make(map[string]bool, len(ns))
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
