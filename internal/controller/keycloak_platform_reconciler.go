// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	networkingv1 "k8s.io/api/networking/v1"
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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const keycloakPlatformReconcileKey = "keycloak-platform"

// KeycloakPlatformReconciler keeps Keycloak iframe policy converged for portal-
// embedded OIDC apps. It patches the shared id.<kernel> ingress and ensures
// browserSecurityHeaders jobs clear X-Frame-Options on all realms.
type KeycloakPlatformReconciler struct {
	client.Client
	KernelDomain string
	TenancyMode  string
	KernelRealm  string
}

func (r *KeycloakPlatformReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	if r.KernelDomain == "" {
		return reconcile.Result{}, nil
	}

	if err := reconcileKeycloakIDPEmbeddingIngress(ctx, r.Client, r.KernelDomain, r.TenancyMode); err != nil {
		logger.Error(err, "Keycloak IdP ingress frame-ancestors reconcile failed")
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

	if !keycloakIngressFramePolicyApplied(ctx, r.Client, r.KernelDomain, r.TenancyMode) {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Re-check periodically so Crossplane/Helm drift on the Nubus ingress is corrected.
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
	ingressName := keycloakProxyIngressName()
	mapToPlatform := func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Name:      keycloakPlatformReconcileKey,
			Namespace: servicesNamespace,
		}}}
	}
	ingressPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			ing, ok := e.Object.(*networkingv1.Ingress)
			return ok && ing.GetName() == ingressName && ing.GetNamespace() == servicesNamespace
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			ing, ok := e.ObjectNew.(*networkingv1.Ingress)
			return ok && ing.GetName() == ingressName && ing.GetNamespace() == servicesNamespace
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			ing, ok := e.Object.(*networkingv1.Ingress)
			return ok && ing.GetName() == ingressName && ing.GetNamespace() == servicesNamespace
		},
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("keycloak-platform").
		For(&networkingv1.Ingress{}, builder.WithPredicates(ingressPredicate)).
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

func reconcileKeycloakIDPEmbeddingIngress(ctx context.Context, c client.Client, kernelDomain, tenancyMode string) error {
	if kernelDomain == "" {
		return nil
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for Keycloak IdP CSP: %w", err)
	}

	var effectiveDomains []string
	var tenantNames []string
	for i := range tenantList.Items {
		if tenantList.Items[i].DeletionTimestamp != nil {
			continue
		}
		if d := tenantList.Items[i].EffectiveDomain(kernelDomain, tenancyMode); d != "" {
			effectiveDomains = append(effectiveDomains, d)
			tenantNames = append(tenantNames, tenantList.Items[i].Name)
		}
	}
	oidcSubs, err := collectOIDCIngressSubdomainsByTenant(ctx, c, tenantList.Items)
	if err != nil {
		return fmt.Errorf("collect OIDC ingress subdomains: %w", err)
	}

	desiredSnippet := keycloakOIDCEmbeddingIngressSnippet(kernelDomain, effectiveDomains, oidcSubs, tenantNames)
	if desiredSnippet == "" {
		return nil
	}

	name := keycloakProxyIngressName()
	existing := &networkingv1.Ingress{}
	err = c.Get(ctx, types.NamespacedName{Name: name, Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		log.FromContext(ctx).Info("Keycloak proxy ingress not found; IdP frame-ancestors will apply when Nubus is ready",
			"ingress", name, "namespace", servicesNamespace)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Keycloak proxy ingress %s/%s: %w", servicesNamespace, name, err)
	}

	desiredServerSnippet := keycloakOIDCIngressServerSnippet()
	annotations := make(map[string]string, len(existing.Annotations)+2)
	for k, v := range existing.Annotations {
		annotations[k] = v
	}
	if annotations[nginxConfigurationSnippetAnnotation] == desiredSnippet &&
		annotations[nginxServerSnippetAnnotation] == desiredServerSnippet {
		return nil
	}
	annotations[nginxConfigurationSnippetAnnotation] = desiredSnippet
	annotations[nginxServerSnippetAnnotation] = desiredServerSnippet

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Annotations = annotations
	if err := c.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("patch Keycloak proxy ingress %s/%s frame-ancestors: %w", servicesNamespace, name, err)
	}
	log.FromContext(ctx).Info("updated Keycloak IdP ingress frame-ancestors for portal-embedded SSO",
		"ingress", name, "namespace", servicesNamespace, "tenantZones", len(effectiveDomains))
	return nil
}

func keycloakIngressFramePolicyApplied(ctx context.Context, c client.Client, kernelDomain, tenancyMode string) bool {
	tenantList := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenantList); err != nil {
		return false
	}
	var effectiveDomains []string
	var tenantNames []string
	for i := range tenantList.Items {
		if tenantList.Items[i].DeletionTimestamp != nil {
			continue
		}
		if d := tenantList.Items[i].EffectiveDomain(kernelDomain, tenancyMode); d != "" {
			effectiveDomains = append(effectiveDomains, d)
			tenantNames = append(tenantNames, tenantList.Items[i].Name)
		}
	}
	oidcSubs, err := collectOIDCIngressSubdomainsByTenant(ctx, c, tenantList.Items)
	if err != nil {
		return false
	}
	desiredSnippet := keycloakOIDCEmbeddingIngressSnippet(kernelDomain, effectiveDomains, oidcSubs, tenantNames)
	if desiredSnippet == "" {
		return true
	}

	existing := &networkingv1.Ingress{}
	if err = c.Get(ctx, types.NamespacedName{
		Name:      keycloakProxyIngressName(),
		Namespace: servicesNamespace,
	}, existing); err != nil {
		return false
	}
	if existing.Annotations[nginxConfigurationSnippetAnnotation] != desiredSnippet {
		return false
	}
	if existing.Annotations[nginxServerSnippetAnnotation] != keycloakOIDCIngressServerSnippet() {
		return false
	}
	if !strings.Contains(existing.Annotations[nginxConfigurationSnippetAnnotation], fmt.Sprintf("https://portal.%s", kernelDomain)) {
		return false
	}
	if !strings.Contains(existing.Annotations[nginxConfigurationSnippetAnnotation], fmt.Sprintf("https://*.%s", kernelDomain)) {
		return false
	}
	for _, d := range effectiveDomains {
		if !strings.Contains(existing.Annotations[nginxConfigurationSnippetAnnotation], fmt.Sprintf("https://*.%s", d)) {
			return false
		}
	}
	for tenantName, subs := range oidcSubs {
		effective := ""
		for i, name := range tenantNames {
			if name == tenantName && i < len(effectiveDomains) {
				effective = effectiveDomains[i]
				break
			}
		}
		if effective == "" {
			continue
		}
		for _, sub := range subs {
			origin := fmt.Sprintf("https://%s.%s", sub, effective)
			if !strings.Contains(existing.Annotations[nginxConfigurationSnippetAnnotation], origin) {
				return false
			}
		}
	}
	return true
}
