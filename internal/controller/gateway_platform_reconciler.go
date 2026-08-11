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
	"fmt"
	"time"

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
)

// GatewayPlatformReconciler ensures cluster-scoped Gateway API foundation
// resources: the shared GatewayClass and kernel-public-gateway.
//
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
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=backendtrafficpolicies;clienttrafficpolicies,verbs=get;list;watch;create;update;patch;delete
type GatewayPlatformReconciler struct {
	client.Client
	KernelDomain  string
	TenancyMode   string
	RoutingMode   string
	CloudflareDNS *CloudflareDNSClient
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
	if err := r.ensureKernelGateway(ctx); err != nil {
		logger.Error(err, "ensure kernel Gateway")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	if err := r.reconcileKernelHTTPRoutes(ctx); err != nil {
		logger.Error(err, "reconcile kernel HTTPRoutes")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	if err := ensureKernelGatewayTunnelIngress(ctx, r.Client, r.CloudflareDNS, r.KernelDomain, r.TenancyMode); err != nil {
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
			return ok && gw.GetName() == KernelPublicGatewayName && gw.GetNamespace() == servicesNamespace
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			gw, ok := e.ObjectNew.(*gatewayv1.Gateway)
			return ok && gw.GetName() == KernelPublicGatewayName && gw.GetNamespace() == servicesNamespace
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			gw, ok := e.Object.(*gatewayv1.Gateway)
			return ok && gw.GetName() == KernelPublicGatewayName && gw.GetNamespace() == servicesNamespace
		},
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}

	configMapPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		cm, ok := obj.(*corev1.ConfigMap)
		return ok && cm.GetNamespace() == operatorNamespace && cm.GetName() == operatorConfigMapName
	})

	envoyKernelServicePredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return false
		}
		if svc.GetNamespace() != envoyGatewayInstallNamespace {
			return false
		}
		return svc.GetLabels()["gateway.envoyproxy.io/owning-gateway-name"] == KernelPublicGatewayName &&
			svc.GetLabels()["gateway.envoyproxy.io/owning-gateway-namespace"] == servicesNamespace
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

func (r *GatewayPlatformReconciler) ensureKernelGateway(ctx context.Context) error {
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for kernel Gateway: %w", err)
	}
	// Tenant ReferenceGrants are owned by Crossplane via each tenant's manifest bridge.
	desired := buildKernelGateway(r.KernelDomain, r.TenancyMode, tenantList.Items)
	return ensureGatewayResource(ctx, r.Client, desired)
}

func buildKernelGateway(kernelDomain, tenancyMode string, tenants []gentianov1alpha1.Tenant) *gatewayv1.Gateway {
	extraListeners := []gatewayv1.Listener{
		kernelApexListener(kernelDomain, kernelWildcardTLSSecretName),
	}
	for i := range tenants {
		tenant := &tenants[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		effectiveDomain := tenant.EffectiveDomain(kernelDomain, tenancyMode)
		if effectiveDomain == "" {
			continue
		}
		nsName := tenantNamespaceName(tenant)
		tlsSecret := tenantWildcardSecretName(tenant.Name)
		extraListeners = append(extraListeners,
			tenantKernelGatewayListener(tenant.Name, effectiveDomain, tlsSecret, nsName, false),
			tenantKernelGatewayListener(tenant.Name, effectiveDomain, tlsSecret, nsName, true),
		)
	}
	return buildGateway(KernelPublicGatewayName, servicesNamespace, kernelDomain, kernelWildcardTLSSecretName, map[string]string{
		managedByLabel:       managedByValue,
		"gentianos.io/scope": "kernel",
	}, gatewayBuildOptions{
		allowCrossNamespaceRoutes: true,
		extraListeners:            extraListeners,
	})
}

func kernelApexListener(kernelDomain, tlsSecret string) gatewayv1.Listener {
	return tlsListener("https-apex", gatewayv1.Hostname(kernelDomain), tlsSecret, servicesNamespace)
}

func tenantKernelGatewayListener(tenantName, effectiveDomain, tlsSecret, tlsSecretNamespace string, apex bool) gatewayv1.Listener {
	name := fmt.Sprintf("https-tenant-%s-wildcard", tenantName)
	hostname := gatewayv1.Hostname(fmt.Sprintf("*.%s", effectiveDomain))
	if apex {
		name = fmt.Sprintf("https-tenant-%s-apex", tenantName)
		hostname = gatewayv1.Hostname(effectiveDomain)
	}
	return tlsListener(name, hostname, tlsSecret, tlsSecretNamespace)
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
	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Protocol: gatewayv1.HTTPSProtocolType,
		Port:     port,
		Hostname: &hostname,
		TLS: &gatewayv1.GatewayTLSConfig{
			Mode: &mode,
			CertificateRefs: []gatewayv1.SecretObjectReference{
				ref,
			},
		},
	}
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

func buildTenantGateway(tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain, tlsSecret string) *gatewayv1.Gateway {
	return buildGateway(tenantGatewayName(tenant.Name), nsName, effectiveDomain, tlsSecret, map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}, gatewayBuildOptions{})
}

func buildGateway(name, namespace, domain, tlsSecret string, labels map[string]string, opts gatewayBuildOptions) *gatewayv1.Gateway {
	hostname := gatewayv1.Hostname(fmt.Sprintf("*.%s", domain))
	listeners := []gatewayv1.Listener{
		withAllowedRoutes(tlsListener("https-wildcard", hostname, tlsSecret, namespace), opts.allowCrossNamespaceRoutes),
		withAllowedRoutes(httpRedirectListener(), opts.allowCrossNamespaceRoutes),
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

	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = desired.Spec
		return c.Patch(ctx, existing, patch)
	}
	return nil
}
