// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"time"

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

	return ctrl.NewControllerManagedBy(mgr).
		Named("gateway-platform").
		For(&gatewayv1.Gateway{}, builder.WithPredicates(gatewayPredicate)).
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
	desired := buildKernelGateway(r.KernelDomain)
	return ensureGatewayResource(ctx, r.Client, desired)
}

func buildKernelGateway(kernelDomain string) *gatewayv1.Gateway {
	return buildGateway(KernelPublicGatewayName, servicesNamespace, kernelDomain, kernelWildcardTLSSecretName, map[string]string{
		managedByLabel:       managedByValue,
		"gentianos.io/scope": "kernel",
	}, nil)
}

func buildTenantGateway(tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain, tlsSecret string) *gatewayv1.Gateway {
	return buildGateway(tenantGatewayName(tenant.Name), nsName, effectiveDomain, tlsSecret, map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}, nil)
}

func buildGateway(name, namespace, domain, tlsSecret string, labels map[string]string, extraListeners []gatewayv1.Listener) *gatewayv1.Gateway {
	hostname := gatewayv1.Hostname(fmt.Sprintf("*.%s", domain))
	port := gatewayv1.PortNumber(443)
	mode := gatewayv1.TLSModeTerminate
	secretKind := gatewayv1.Kind("Secret")

	listeners := []gatewayv1.Listener{
		{
			Name:     "https-wildcard",
			Protocol: gatewayv1.HTTPSProtocolType,
			Port:     port,
			Hostname: &hostname,
			TLS: &gatewayv1.GatewayTLSConfig{
				Mode: &mode,
				CertificateRefs: []gatewayv1.SecretObjectReference{
					{
						Kind: &secretKind,
						Name: gatewayv1.ObjectName(tlsSecret),
					},
				},
			},
		},
	}
	listeners = append(listeners, extraListeners...)

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
