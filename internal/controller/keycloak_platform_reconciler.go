// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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

const keycloakPlatformReconcileKey = "keycloak-platform"

// KeycloakPlatformReconciler keeps Keycloak iframe policy converged for portal-
// embedded OIDC apps. It patches the shared id.<kernel> HTTPRoute and ensures
// browserSecurityHeaders jobs clear X-Frame-Options on all realms.
type KeycloakPlatformReconciler struct {
	client.Client
	KernelDomain string
	TenancyMode  string
	KernelRealm  string
	RoutingMode  string
}

func (r *KeycloakPlatformReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	if r.KernelDomain == "" {
		return reconcile.Result{}, nil
	}

	if err := reconcileKeycloakIDPGatewayRoute(ctx, r.Client, r.KernelDomain, r.TenancyMode); err != nil {
		logger.Error(err, "Keycloak IdP HTTPRoute frame-ancestors reconcile failed")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}

	ready, err := r.ensureAllBrowserSecurityHeaderJobs(ctx)
	if err != nil {
		logger.Error(err, "Keycloak browser security header jobs failed")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}
	if !ready {
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if !keycloakGatewayFramePolicyApplied(ctx, r.Client, r.KernelDomain, r.TenancyMode) {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Re-check periodically so Crossplane/Helm drift on kernel HTTPRoutes is corrected.
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *KeycloakPlatformReconciler) ensureAllBrowserSecurityHeaderJobs(ctx context.Context) (bool, error) {
	kernelRealm := r.KernelRealm
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	tr := &TenantReconciler{
		Client:      r.Client,
		KernelRealm: kernelRealm,
	}
	if err := tr.ensureBrowserSecurityHeadersJob(ctx, kernelBrowserSecurityJobName(), kernelRealm); err != nil {
		return false, err
	}
	kernelJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: kernelBrowserSecurityJobName(), Namespace: kernelNamespace}, kernelJob); err != nil {
		return false, err
	}
	if !jobIsComplete(kernelJob) {
		return false, nil
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return false, fmt.Errorf("list tenants for browser security jobs: %w", err)
	}
	for i := range tenantList.Items {
		tenant := &tenantList.Items[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		jobName := tenantBrowserSecurityJobName(tenant.Name)
		realm := keycloakRealmName(tenant)
		if err := tr.ensureBrowserSecurityHeadersJob(ctx, jobName, realm); err != nil {
			return false, err
		}
		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if !jobIsComplete(job) {
			return false, nil
		}
	}
	return true, nil
}

func (r *KeycloakPlatformReconciler) SetupWithManager(mgr ctrl.Manager) error {
	routeName := kernelKeycloakHTTPRouteName()
	mapToPlatform := func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Name:      keycloakPlatformReconcileKey,
			Namespace: servicesNamespace,
		}}}
	}
	routePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			route, ok := e.Object.(*gatewayv1.HTTPRoute)
			return ok && route.GetName() == routeName && route.GetNamespace() == servicesNamespace
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			route, ok := e.ObjectNew.(*gatewayv1.HTTPRoute)
			return ok && route.GetName() == routeName && route.GetNamespace() == servicesNamespace
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			route, ok := e.Object.(*gatewayv1.HTTPRoute)
			return ok && route.GetName() == routeName && route.GetNamespace() == servicesNamespace
		},
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("keycloak-platform").
		For(&gatewayv1.HTTPRoute{}, builder.WithPredicates(routePredicate)).
		Watches(
			&gentianov1alpha1.Tenant{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
		).
		Watches(
			&gentianov1alpha1.AppProfile{},
			handler.EnqueueRequestsFromMapFunc(mapToPlatform),
		).
		Complete(r)
}
