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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/hostnames"
)

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;gateways/status;httproutes;httproutes/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;create;update;patch
//
// ReferenceGrants authorise cross-namespace HTTPRoute -> Service references
// (e.g. the kernel Gateway routing to Argo CD). gateway_reference_grant.go
// creates them and tenant_edge_tls.go deletes them on teardown. A missing grant
// does not crash the operator — it just never converges, failing every pass
// with "ensure ArgoCD ReferenceGrant: referencegrants... is forbidden".
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete
//
// ClientTrafficPolicy sits alongside BackendTrafficPolicy: the kernel routes
// reconciler creates them and tenant cleanup lists them for stale removal.
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=backendtrafficpolicies;clienttrafficpolicies;securitypolicies,verbs=get;list;watch;create;update;patch;delete
//
// Deployments back the CoreDNS hairpin (coredns_hairpin.go): the ConfigMap name
// is discovered from the volume the CoreDNS Deployment mounts, and the
// Deployment is then patched to restart CoreDNS after the Corefile changes. The
// apps group was absent from the ClusterRole entirely, so restartCoreDNSDeployment
// could never have worked. list+watch accompany get because the manager's client
// reads through its cache, so a Get starts an informer that must be able to list.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch

// GatewayPlatformReconciler ensures cluster-scoped Gateway API foundation
// resources: the shared GatewayClass and the two edge Gateways.
type GatewayPlatformReconciler struct {
	client.Client
	KernelDomain string
	TenancyMode  string
	RoutingMode  string
	Ingress      EdgeIngress
	// Cluster is this cluster's id in the authorization store: the object
	// the kernel UIs' relations are asked on (cluster:<c>).
	Cluster string
	// KernelRealm is the realm the kernel zone's session is established in.
	KernelRealm string
	// BouncerService is the ext-auth bouncer's Service in the edge namespace.
	BouncerService string
}

func (r *GatewayPlatformReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	if !isGatewayRoutingMode(r.RoutingMode) || r.KernelDomain == "" {
		return reconcile.Result{}, nil
	}

	if err := r.ensureGatewayClass(ctx); err != nil {
		logger.Error(err, "ensure GatewayClass")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	if err := r.ensureEdgeGateways(ctx); err != nil {
		logger.Error(err, "ensure edge Gateways")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	if err := r.reconcileKernelHTTPRoutes(ctx); err != nil {
		logger.Error(err, "reconcile kernel HTTPRoutes")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	if err := ensureKernelGatewayTunnelIngress(ctx, r.Client, r.Ingress, r.KernelDomain, r.TenancyMode, r.kernelRealm()); err != nil {
		logger.Error(err, "ensure kernel Cloudflare tunnel ingress")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	if err := ensureCoreDNSHairpin(ctx, r.Client, r.KernelDomain, r.TenancyMode, r.RoutingMode); err != nil {
		logger.Error(err, "reconcile CoreDNS kernel hairpin")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}

	return reconcile.Result{}, nil
}

func (r *GatewayPlatformReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if !isGatewayRoutingMode(r.RoutingMode) {
		return nil
	}

	mapToPlatform := func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Name:      gatewayPlatformReconcileKey,
			Namespace: servicesNamespace,
		}}}
	}

	gatewayPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			gw, ok := e.Object.(*gatewayv1.Gateway)
			return ok && isEdgeGateway(gw)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			gw, ok := e.ObjectNew.(*gatewayv1.Gateway)
			return ok && isEdgeGateway(gw)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			gw, ok := e.Object.(*gatewayv1.Gateway)
			return ok && isEdgeGateway(gw)
		},
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}

	configMapPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		cm, ok := obj.(*corev1.ConfigMap)
		return ok && cm.GetNamespace() == operatorNamespace && cm.GetName() == operatorConfigMapName
	})

	envoyKernelServicePredicate := predicate.NewPredicateFuncs(isKernelEdgeService)
	// The kernel zone's client secret: the kernel UIs are routed only once
	// it exists, so its arrival is what routes them.
	zoneSecretPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		s, ok := obj.(*corev1.Secret)
		return ok && s.GetNamespace() == servicesNamespace && s.GetName() == edgeKernelSecretName
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("gateway-platform").
		For(&corev1.ConfigMap{}, builder.WithPredicates(configMapPredicate)).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
			builder.WithPredicates(gatewayPredicate),
		).
		Watches(
			&gentianov1alpha1.Tenant{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
		).
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
			builder.WithPredicates(envoyKernelServicePredicate),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
			builder.WithPredicates(zoneSecretPredicate),
		).
		// A component's key for the rights check: the bouncer's table must
		// hold its hash, and stop holding it when the Secret goes.
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetLabels()[rightsCheckerLabel] == "true"
			})),
		).
		// A tenant realm's exchange client secret: the bouncer presents it,
		// from the one Secret this reconciler gathers them into.
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, ok := obj.GetLabels()[exchangeRealmLabel]
				return ok && obj.GetNamespace() == servicesNamespace
			})),
		).
		// A component's route carries a question the bouncer's table must hold.
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetLabels()[bouncerRouteLabel] == "true"
			})),
		).
		// A publishing proxy coming up or going away: the front page of the
		// main address follows the website's (mainAddressWebsiteServing).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetLabels()["app.kubernetes.io/name"] == perimeterProxyAppName
			})),
		).
		Complete(r)
}

func (r *GatewayPlatformReconciler) ensureGatewayClass(ctx context.Context) error {
	desc := "Gentian OS edge routing via Envoy Gateway"
	desired := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: GentianGatewayClassName,
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(GentianGatewayControllerName),
			Description:    &desc,
		},
	}

	existing := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: GentianGatewayClassName}, existing); err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}

	if existing.Spec.ControllerName != desired.Spec.ControllerName {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.ControllerName = desired.Spec.ControllerName
		if existing.Spec.Description == nil {
			existing.Spec.Description = desired.Spec.Description
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func isEdgeGateway(gw *gatewayv1.Gateway) bool {
	return gw.GetNamespace() == servicesNamespace &&
		(gw.GetName() == AuthenticatedGatewayName || gw.GetName() == PerimeterGatewayName)
}

// ensureEdgeGateways reconciles the two Gateways of the edge from the
// cluster's state: the authenticated one with the kernel wildcard and a
// listener per tenant zone, the perimeter one with the identity provider's
// hostname and the :80 listener the ACME challenge and the https redirect
// share. Both are kernel resources -- a tenant owns HTTPRoutes, never a
// Gateway, because listener uniqueness is class-wide once gateways merge.
func (r *GatewayPlatformReconciler) ensureEdgeGateways(ctx context.Context) error {
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for the edge Gateways: %w", err)
	}
	// A published host is the entry's own, which the profile says. Components
	// are named after their profile, so the install names both.
	profileList := &gentianov1alpha1.ComponentProfileList{}
	if err := r.List(ctx, profileList); err != nil {
		return fmt.Errorf("list profiles for the perimeter Gateway: %w", err)
	}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{}
	for i := range profileList.Items {
		profiles[profileList.Items[i].Name] = &profileList.Items[i]
	}
	ann := edgeDNSAnnotations(r.Ingress)
	for _, desired := range []*gatewayv1.Gateway{
		buildAuthenticatedGateway(r.KernelDomain, r.TenancyMode, tenantList.Items),
		buildPerimeterGateway(r.KernelDomain, r.TenancyMode, r.kernelRealm(), tenantList.Items, profiles),
	} {
		if len(ann) > 0 {
			if desired.Annotations == nil {
				desired.Annotations = map[string]string{}
			}
			for k, v := range ann {
				desired.Annotations[k] = v
			}
		}
		if err := ensureGatewayResource(ctx, r.Client, desired); err != nil {
			return fmt.Errorf("ensure Gateway %s: %w", desired.Name, err)
		}
	}
	return nil
}

// buildPerimeterGateway is the edge with no session: the identity provider's
// own hostname on :443 with the kernel wildcard certificate, and :80, where
// the ACME HTTP-01 solver answers and everything else is redirected to https.
// Under mergeGateways a listener is unique per port and hostname across the
// class, so :80 lives here and nowhere else.
func buildPerimeterGateway(kernelDomain, tenancyMode, kernelRealm string, tenants []gentianov1alpha1.Tenant, profiles map[string]*gentianov1alpha1.ComponentProfile) *gatewayv1.Gateway {
	idHost := gatewayv1.Hostname("id." + kernelDomain)
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PerimeterGatewayName,
			Namespace: servicesNamespace,
			Labels: map[string]string{
				managedByLabel:       managedByValue,
				"gentianos.io/scope": "kernel",
				"gentianos.io/edge":  "perimeter",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(GentianGatewayClassName),
			Listeners: append([]gatewayv1.Listener{
				withAllowedRoutes(tlsListener(perimeterIDListenerName, idHost, kernelWildcardTLSSecretName, servicesNamespace), true),
				withAllowedRoutes(httpRedirectListener(), true),
			}, perimeterTenantListeners(kernelDomain, tenancyMode, kernelRealm, tenants, profiles)...),
		},
	}
}

// perimeterTenantListeners is one listener per host a tenant publishes.
//
// A published host needs a listener or its route attaches to nothing, and
// "the route exists but answers nothing" is the failure mode with no error to
// read: the HTTPRoute reports NoMatchingParent in a status nobody is watching.
//
// EXACT hostnames, not a wildcard. Two reasons, and the second is the one
// that matters. Under mergeGateways a listener is unique per port and
// hostname across the class, and the authenticated Gateway already holds
// *.<tenant-domain> -- so a wildcard here would collide with it. And an exact
// hostname is what makes the perimeter narrow: a tenant publishes
// share.acme.example and that host alone leaves the session behind, while
// everything else on *.acme.example stays on the authenticated edge where it
// was. Gateway API prefers the more specific listener, so the two coexist and
// only the published name is public.
//
// The certificate is the tenant's own wildcard, which already covers any
// subdomain of its domain, so publishing a surface issues nothing new.
func perimeterTenantListeners(kernelDomain, tenancyMode, kernelRealm string, tenants []gentianov1alpha1.Tenant, profiles map[string]*gentianov1alpha1.ComponentProfile) []gatewayv1.Listener {
	var out []gatewayv1.Listener
	seen := map[string]struct{}{}
	for i := range tenants {
		tenant := &tenants[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		zone := zoneNamesOf(tenant, kernelDomain, tenancyMode, kernelRealm)
		for j := range tenant.Spec.Exposures {
			host := publishedHost(&tenant.Spec.Exposures[j], profiles, zone, kernelDomain)
			if host == "" {
				continue
			}
			// The certificate that names the host. One directly under the
			// cluster's domain, or that domain itself, is on the cluster's
			// own: the bare domain the platform tenant publishes, and every
			// host of the user tenant of a single-tenancy cluster. Anything
			// deeper is on the tenant's wildcard.
			secret, secretNamespace := tenantWildcardSecretName(tenant.Name), tenantNamespaceName(tenant)
			if directlyUnder(host, kernelDomain) {
				secret, secretNamespace = kernelWildcardTLSSecretName, servicesNamespace
			}
			if _, dup := seen[host]; dup {
				continue
			}
			seen[host] = struct{}{}
			out = append(out, withAllowedRoutes(tlsListener(
				perimeterListenerName(host), gatewayv1.Hostname(host), secret, secretNamespace,
			), true))
		}
	}
	// A tenant's website on the cluster's main address answers on the bare
	// domain, with the cluster's own certificate. The platform's page is
	// normally published there already and this adds nothing; where it is
	// not, the website still needs the listener.
	if holder := mainAddressHolder(mainAddressInputs{
		Tenants: tenants, Profiles: profiles,
		KernelDomain: kernelDomain, KernelRealm: kernelRealm, TenancyMode: tenancyMode, Now: time.Now(),
	}); holder != nil {
		if _, dup := seen[kernelDomain]; !dup {
			out = append(out, withAllowedRoutes(tlsListener(
				perimeterListenerName(kernelDomain), gatewayv1.Hostname(kernelDomain),
				kernelWildcardTLSSecretName, servicesNamespace,
			), true))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// publishedHost is where one enablement answers, by the same rule the route
// behind it is written with (exposureHostIn): the listener and the route are
// two halves of one host and must not each have an opinion about its name.
//
// An enablement naming a profile or an entry that does not exist, or an entry
// that is not a perimeter one, has no host and gets no listener. Nor has an
// approval of another kind than the entry declares: the approval of an entry
// behind sign-in that keeps the app's own Authorization header publishes
// nothing, and neither does one given before the entry asked for more.
//
// An expired enablement still gets a listener. A listener with no route
// behind it serves nothing -- the proxy and the route are what the operator
// takes down at expiry -- and keeping it means a renewal does not have to
// wait for the Gateway to be reprogrammed before the link works again.
//
// A component held for an address name the platform keeps (HostReserved)
// gets no listener either. An exact-hostname listener is preferred over the
// zone's wildcard, so one for desktop.<tenant> here would take the desktop's
// host off the authenticated Gateway although nothing is routed behind it.
func publishedHost(e *gentianov1alpha1.TenantExposure, profiles map[string]*gentianov1alpha1.ComponentProfile, zone zoneNames, kernelDomain string) string {
	profile := profiles[e.Install]
	if profile == nil {
		return ""
	}
	if hostnames.Check(e.Install, profile, reservedHostZone(zone, kernelDomain)) != nil {
		return ""
	}
	for i := range profile.Spec.Expose {
		entry := &profile.Spec.Expose[i]
		if entry.Name == e.ExposureName && entry.Surface == gentianov1alpha1.SurfacePerimeter && entry.ApprovedAs(e.Kind) {
			return exposureHostIn(zone, e.Install, entry)
		}
	}
	return ""
}

// perimeterListenerName is a Gateway listener name derived from the host:
// a DNS label, unique, and within the 253 the API allows.
func perimeterListenerName(host string) string {
	name := "perimeter-" + strings.ReplaceAll(host, ".", "-")
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(host))
	return "perimeter-" + hex.EncodeToString(sum[:])[:16]
}

// buildAuthenticatedGateway is the edge behind a session: the catch-all
// listener with the cluster's own certificate, and a listener per tenant whose
// hosts are deeper than one label under the cluster's domain. That is every
// tenant with a domain of its own, and the platform tenant, whose consoles
// are at <label>.platform.<kernel>. It is not the user tenant of a
// single-tenancy cluster: its hosts are the catch-all's.
func buildAuthenticatedGateway(kernelDomain, tenancyMode string, tenants []gentianov1alpha1.Tenant) *gatewayv1.Gateway {
	// No apex listener. The catch-all HTTPS listener already serves gtn.host —
	// the kernel certificate carries it alongside *.gtn.host — and a separate
	// listener scoped to the apex only served to make portal.gtn.host
	// unroutable on connections a browser had coalesced onto gtn.host.
	var extraListeners []gatewayv1.Listener
	for i := range tenants {
		tenant := &tenants[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		effectiveDomain := tenant.EffectiveDomain(kernelDomain, tenancyMode)
		if effectiveDomain == "" || servedByKernelEdge(effectiveDomain, kernelDomain) {
			continue
		}
		nsName := tenantNamespaceName(tenant)
		tlsSecret := tenantWildcardSecretName(tenant.Name)
		// Subdomains only. <tenant>.<kernel-domain> is already covered by the
		// kernel certificate and served by the catch-all listener; giving it its
		// own narrow listener reintroduced the coalescing hole one level down,
		// where a connection opened for the tenant apex could not route
		// <app>.<tenant>.<kernel-domain>.
		extraListeners = append(extraListeners,
			tenantKernelGatewayListener(tenant.Name, effectiveDomain, tlsSecret, nsName),
		)
	}
	return buildGateway(AuthenticatedGatewayName, servicesNamespace, kernelDomain, kernelWildcardTLSSecretName, map[string]string{
		managedByLabel:       managedByValue,
		"gentianos.io/scope": "kernel",
		"gentianos.io/edge":  "authenticated",
	}, gatewayBuildOptions{
		allowCrossNamespaceRoutes: true,
		extraListeners:            extraListeners,
	})
}

// Listener names are the binding target for every route's parentRef
// sectionName. A content route that omits sectionName attaches to EVERY
// listener whose hostname matches — including the plaintext :80 redirect
// listener, which is hostname-less and therefore matches everything. Gateway
// API then ranks the content route's specific hostname above the redirect
// route's absent one, so http://<any-host> was served in the clear instead of
// being redirected. Every content route must name its HTTPS listener.
const (
	wildcardListenerName    = "https-wildcard"
	perimeterIDListenerName = "https-id"
)

// tenantGatewayListenerName is the kernel-Gateway listener carrying a tenant's
// own certificate, for the tenant's apex or its wildcard subdomains.
// tenantGatewayListenerName is the kernel-Gateway listener carrying a tenant's
// own certificate for its subdomains. There is no apex variant: the tenant apex
// is covered by the kernel certificate and served by the catch-all listener.
// servedByKernelEdge reports a tenant whose domain is the kernel domain: the
// user tenant of a single-tenancy cluster. The kernel's catch-all listener,
// certificate and DNS already cover every name under it, so the tenant gets
// none of its own -- a *.<kernel> tenant listener would be the more specific
// match for kernel hosts too, and route them nowhere.
func servedByKernelEdge(effectiveDomain, kernelDomain string) bool {
	return kernelDomain != "" && effectiveDomain == kernelDomain
}

func tenantGatewayListenerName(tenantName string) string {
	return fmt.Sprintf("https-tenant-%s-wildcard", tenantName)
}

func tenantKernelGatewayListener(tenantName, effectiveDomain, tlsSecret, tlsSecretNamespace string) gatewayv1.Listener {
	hostname := gatewayv1.Hostname(fmt.Sprintf("*.%s", effectiveDomain))
	return tlsListener(tenantGatewayListenerName(tenantName), hostname, tlsSecret, tlsSecretNamespace)
}

func tlsListener(name string, hostname gatewayv1.Hostname, tlsSecret, tlsSecretNamespace string) gatewayv1.Listener {
	port := gatewayv1.PortNumber(443)
	mode := gatewayv1.TLSModeTerminate
	secretKind := gatewayv1.Kind("Secret")
	ref := gatewayv1.SecretObjectReference{
		Kind: &secretKind,
		Name: gatewayv1.ObjectName(tlsSecret),
	}
	if tlsSecretNamespace != "" && tlsSecretNamespace != servicesNamespace {
		ns := gatewayv1.Namespace(tlsSecretNamespace)
		ref.Namespace = &ns
	}
	l := gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Protocol: gatewayv1.HTTPSProtocolType,
		Port:     port,
		TLS: &gatewayv1.GatewayTLSConfig{
			Mode: &mode,
			CertificateRefs: []gatewayv1.SecretObjectReference{
				ref,
			},
		},
	}
	// An empty hostname makes the listener match every SNI the certificate
	// covers. A listener's hostname does double duty in Gateway API: it selects
	// the listener by SNI AND gates which routes may attach, and a route only
	// attaches where its hostnames intersect the listener's.
	//
	// Splitting one certificate across narrow listeners therefore breaks HTTP/2
	// connection coalescing. The kernel certificate carries both gtn.host and
	// *.gtn.host, so a browser may legitimately reuse the gtn.host connection
	// for portal.gtn.host; the request then arrives on the apex listener, where
	// portal.gtn.host could never attach, and Envoy answers 404 with no route.
	// It reads as random because coalescing depends on which connection is open:
	// arriving via the apex redirect coalesces, opening the host directly does
	// not, and curl never reproduces it because it opens one connection per host.
	if hostname != "" {
		l.Hostname = &hostname
	}
	return l
}

// httpRedirectListenerName is the plaintext listener that exists solely so
// http:// requests can be answered with a permanent redirect to https://.
//
// Serving nothing on :80 is the unusual choice: a browser given a bare hostname
// tries http:// first, and with no listener that is a connection refusal, which
// is indistinguishable from an outage. It also blocks HSTS preload (which
// requires the redirect to exist) and ACME HTTP-01, should DNS-01 ever be
// unavailable.
const httpRedirectListenerName = "http-redirect"

// httpRedirectListener is deliberately hostname-less so it matches every host
// arriving on :80 — apex, wildcard and tenant domains alike — and needs no
// updating as domains come and go. The redirect itself lives in an HTTPRoute
// bound to this listener by sectionName; see kernelHTTPRedirectRouteSpec.
func httpRedirectListener() gatewayv1.Listener {
	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(httpRedirectListenerName),
		Protocol: gatewayv1.HTTPProtocolType,
		Port:     gatewayv1.PortNumber(80),
	}
}

type gatewayBuildOptions struct {
	allowCrossNamespaceRoutes bool
	extraListeners            []gatewayv1.Listener
}

func buildGateway(name, namespace, domain, tlsSecret string, labels map[string]string, opts gatewayBuildOptions) *gatewayv1.Gateway {
	// No hostname: this listener serves every name its certificate covers, which
	// is what keeps connection coalescing working (see tlsListener).
	listeners := []gatewayv1.Listener{
		withAllowedRoutes(tlsListener(wildcardListenerName, "", tlsSecret, namespace), opts.allowCrossNamespaceRoutes),
	}
	for i := range opts.extraListeners {
		listeners = append(listeners, withAllowedRoutes(opts.extraListeners[i], opts.allowCrossNamespaceRoutes))
	}

	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(GentianGatewayClassName),
			Listeners:        listeners,
		},
	}
}

func withAllowedRoutes(listener gatewayv1.Listener, crossNamespace bool) gatewayv1.Listener {
	from := gatewayv1.NamespacesFromSame
	if crossNamespace {
		from = gatewayv1.NamespacesFromAll
	}
	listener.AllowedRoutes = &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{
			From: &from,
		},
	}
	return listener
}

func ensureGatewayResource(ctx context.Context, c client.Client, desired *gatewayv1.Gateway) error {
	existing := &gatewayv1.Gateway{}
	err := c.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Annotations as well as spec. The external-dns target lives here and
	// changes whenever the tunnel is rebuilt under a new id; a spec-only
	// comparison would carry the old target for the life of the Gateway, and
	// every hostname would resolve to a tunnel that no longer exists.
	//
	// Merged rather than replaced: other controllers annotate this object too,
	// and reconciling ours must not delete theirs.
	annotationsDiffer := false
	for k, v := range desired.Annotations {
		if existing.Annotations[k] != v {
			annotationsDiffer = true
			break
		}
	}
	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) || annotationsDiffer {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = desired.Spec
		if annotationsDiffer {
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			for k, v := range desired.Annotations {
				existing.Annotations[k] = v
			}
		}
		return c.Patch(ctx, existing, patch)
	}
	return nil
}
