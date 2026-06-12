// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// GatewayPlatformReconciler ensures cluster-scoped Gateway API foundation
// resources: the shared GatewayClass and kernel-public-gateway.
type GatewayPlatformReconciler struct {
	client.Client
	KernelDomain string
	TenancyMode  string
	RoutingMode  string
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
	if err := r.deleteSupersededKernelIngress(ctx); err != nil {
		logger.Error(err, "delete superseded kernel Ingress resources")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}

	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
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
	for i := range tenantList.Items {
		tenant := &tenantList.Items[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		if err := ensureTenantKernelGatewayReferenceGrants(ctx, r.Client, tenant); err != nil {
			return fmt.Errorf("ensure ReferenceGrants for tenant %s: %w", tenant.Name, err)
		}
	}
	desired := buildKernelGateway(r.KernelDomain, r.TenancyMode, tenantList.Items)
	return ensureGatewayResource(ctx, r.Client, desired)
}

func buildKernelGateway(kernelDomain, tenancyMode string, tenants []gentianov1alpha1.Tenant) *gatewayv1.Gateway {
	extraListeners := []gatewayv1.Listener{kernelApexListener(kernelDomain, kernelWildcardTLSSecretName)}
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

// deleteSupersededKernelIngress removes legacy nginx Ingress objects for kernel
// hosts once Gateway API routes are in place. Chart-managed Ingress may be
// recreated until gateway Helm overlays are applied; this keeps the cluster
// converged on kernel-public-gateway.
func (r *GatewayPlatformReconciler) deleteSupersededKernelIngress(ctx context.Context) error {
	list := &networkingv1.IngressList{}
	if err := r.List(ctx, list, client.InNamespace(servicesNamespace)); err != nil {
		return fmt.Errorf("list kernel Ingress resources: %w", err)
	}

	logger := log.FromContext(ctx)
	var deleted int
	for i := range list.Items {
		ing := &list.Items[i]
		if !kernelIngressSupersededByGateway(ing, r.KernelDomain) {
			continue
		}
		if err := r.Delete(ctx, ing); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete superseded kernel Ingress %s: %w", ing.Name, err)
		}
		deleted++
	}
	if deleted > 0 {
		logger.Info("deleted legacy kernel Ingress superseded by Gateway API",
			"namespace", servicesNamespace, "count", deleted)
	}
	return nil
}

func kernelIngressSupersededByGateway(ing *networkingv1.Ingress, kernelDomain string) bool {
	if ing.GetNamespace() != servicesNamespace || kernelDomain == "" {
		return false
	}
	for _, rule := range ing.Spec.Rules {
		host := strings.ToLower(rule.Host)
		if host == "" {
			continue
		}
		if host == kernelDomain || strings.HasSuffix(host, "."+kernelDomain) {
			return true
		}
	}
	return false
}
