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
// embedded OIDC apps. It patches the shared id.<kernel> HTTPRoute and applies
// browserSecurityHeaders on all realms via the Admin REST API.
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

	if err := r.ensureAllBrowserSecurityHeaders(ctx); err != nil {
		logger.Error(err, "Keycloak browser security headers failed")
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}

	if !keycloakGatewayFramePolicyApplied(ctx, r.Client, r.KernelDomain, r.TenancyMode) {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *KeycloakPlatformReconciler) ensureAllBrowserSecurityHeaders(ctx context.Context) error {
	kernelRealm := r.KernelRealm
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	tr := &TenantReconciler{
		Client:      r.Client,
		KernelRealm: kernelRealm,
	}
	if err := tr.ensureRealmBrowserSecurityHeaders(ctx, kernelRealm); err != nil {
		return fmt.Errorf("kernel browser security headers: %w", err)
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for browser security headers: %w", err)
	}
	for i := range tenantList.Items {
		tenant := &tenantList.Items[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		if err := tr.ensureRealmBrowserSecurityHeaders(ctx, keycloakRealmName(tenant)); err != nil {
			return fmt.Errorf("tenant %s browser security headers: %w", tenant.Name, err)
		}
		tr.deleteRetiredJobs(ctx, tenantBrowserSecurityJobName(tenant.Name))
	}
	tr.deleteRetiredJobs(ctx, kernelBrowserSecurityJobName())
	return nil
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
