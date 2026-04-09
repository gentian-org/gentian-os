/*
Copyright 2026 The Gentian Authors.

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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	tenantFinalizer = "gentianos.io/tenant-cleanup"
	tenantLabel     = "gentianos.io/tenant"
	managedByLabel  = "app.kubernetes.io/managed-by"
	managedByValue  = "gentian-os"
	kernelNamespace = "platform-kernel"
	// infraNamespace is the shared infrastructure namespace hosting MariaDB, Redis,
	// and MinIO. Tenant-egress NetworkPolicy rules must allow traffic here so apps
	// can reach their datastores. This is environment-specific (gentian-infra-{env})
	// and should be made configurable via an operator env var / Helm value.
	// TODO: read from INFRA_NAMESPACE env var (defaulting to this constant).
	infraNamespace = "gentian-infra-dev"
	// ingressNamespace is the namespace where the nginx ingress controller runs.
	// Pods in this namespace must be allowed ingress to tenant pods so that the
	// controller can proxy external requests to services inside the tenant namespace.
	ingressNamespace = "ingress"
	conditionNamespaceReady = "NamespaceReady"
)

// TenantReconciler reconciles Tenant objects.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=appprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the controller with the controller-manager.
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapToTenant maps any labelled object back to a reconcile request for the owning Tenant.
	mapToTenant := func(_ context.Context, obj client.Object) []reconcile.Request {
		tenantName := obj.GetLabels()[tenantLabel]
		if tenantName == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: tenantName}}}
	}

	// cnpgDB is an unstructured object used to watch CloudNativePG Database CRs
	// across all tenant namespaces. Status updates (Ready=True) trigger reconciliation
	// to advance the database provisioning sequence.
	cnpgDB := &unstructured.Unstructured{}
	cnpgDB.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind,
	})

	// argocdApp watches ArgoCD Application CRs so that Memcached health transitions
	// immediately trigger re-reconciliation rather than waiting for the requeue timer.
	argocdApp := &unstructured.Unstructured{}
	argocdApp.SetGroupVersionKind(argocdApplicationGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.Tenant{}).
		Owns(&corev1.Namespace{}).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(mapToTenant),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, hasLabel := obj.GetLabels()[tenantLabel]
				return hasLabel && obj.GetNamespace() == kernelNamespace
			})),
		).
		Watches(
			cnpgDB,
			handler.EnqueueRequestsFromMapFunc(mapToTenant),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, hasLabel := obj.GetLabels()[tenantLabel]
				return hasLabel
			})),
		).
		Watches(
			argocdApp,
			handler.EnqueueRequestsFromMapFunc(mapToTenant),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, hasLabel := obj.GetLabels()[tenantLabel]
				return hasLabel && obj.GetNamespace() == argocdNamespace
			})),
		).
		Complete(r)
}

// Reconcile is the main reconciliation loop for Tenant resources.
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	start := time.Now()

	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		reconcileErrors.WithLabelValues("tenant").Inc()
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !tenant.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, tenant)
	}

	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(tenant, tenantFinalizer) {
		controllerutil.AddFinalizer(tenant, tenantFinalizer)
		if err := r.Update(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	nsName := tenantNamespaceName(tenant)
	logger.Info("reconciling tenant", "tenant", tenant.Name, "namespace", nsName)

	// 1. Namespace
	if err := r.ensureNamespace(ctx, tenant, nsName); err != nil {
		r.setCondition(tenant, conditionNamespaceReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 2. ResourceQuota
	if err := r.ensureResourceQuota(ctx, tenant, nsName); err != nil {
		return ctrl.Result{}, err
	}

	// 3. LimitRange
	if err := r.ensureLimitRange(ctx, tenant, nsName); err != nil {
		return ctrl.Result{}, err
	}

	// 4. NetworkPolicy
	if err := r.ensureNetworkPolicy(ctx, tenant, nsName); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Identity (Keycloak realm + OIDC clients)
	identityResult, err := r.ensureIdentity(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 6. LDAP (UDM OU + groups + bind accounts)
	ldapResult, err := r.ensureLDAP(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 7. Database (CloudNativePG Database CRs + psql role Jobs)
	databaseResult, err := r.ensureDatabase(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionDatabaseReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 8. MariaDB (Job-based CREATE DATABASE + CREATE USER + GRANT)
	mariadbResult, err := r.ensureMariaDB(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionMariaDBReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 9. Storage (MinIO S3 buckets + Nextcloud groups)
	storageResult, err := r.ensureStorage(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 10. Cache (Redis ACL users + Memcached ArgoCD Applications)
	cacheResult, err := r.ensureCache(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionCacheReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 11. App deployment (ArgoCD Application CRs per app)
	appsResult, err := r.ensureAppDeployment(ctx, tenant)
	if err != nil {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 12. Ingress (per-app Ingress + per-tenant wildcard cert-manager Certificate)
	if _, err := r.ensureIngress(ctx, tenant); err != nil {
		r.setCondition(tenant, conditionIngressReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 13. Integration bindings (auto-wire provider+consumer pairs within the tenant)
	if _, err := r.ensureIntegrationBindings(ctx, tenant); err != nil {
		r.setCondition(tenant, conditionBindingsReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 14. Mail kernel extension (Postfix + Dovecot per-tenant, or external/relay/disabled)
	mailResult, err := r.ensureMail(ctx, tenant)
	if err != nil {
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 15. Update status
	r.setCondition(tenant, conditionNamespaceReady, metav1.ConditionTrue, "Provisioned", "Tenant namespace is ready")
	tenant.Status.Namespace = nsName
	tenant.Status.AppCount = len(tenant.Spec.Apps)
	tenant.Status.ReadyApps = len(tenant.Status.ProvisionedApps)
	provisioning := identityResult.RequeueAfter > 0 || ldapResult.RequeueAfter > 0 ||
		databaseResult.RequeueAfter > 0 || mariadbResult.RequeueAfter > 0 ||
		storageResult.RequeueAfter > 0 || cacheResult.RequeueAfter > 0
	// Note: mailResult is intentionally excluded from the provisioning flag.
	// Mail is an extension (like app deployment) and does not block Phase=Ready.
	// Its own MailReady condition tracks mail-specific state independently.
	if provisioning {
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseProvisioning
	} else {
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseReady
		provisioningDuration.WithLabelValues(tenant.Name).Observe(time.Since(start).Seconds())
	}
	// Update Prometheus gauges for this tenant.
	tenantAppsTotal.WithLabelValues(tenant.Name).Set(float64(tenant.Status.AppCount))
	if err := r.Status().Update(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	if provisioning {
		logger.Info("tenant provisioning in progress", "tenant", tenant.Name)
		if identityResult.RequeueAfter > 0 {
			return identityResult, nil
		}
		if ldapResult.RequeueAfter > 0 {
			return ldapResult, nil
		}
		if databaseResult.RequeueAfter > 0 {
			return databaseResult, nil
		}
		if mariadbResult.RequeueAfter > 0 {
			return mariadbResult, nil
		}
		if storageResult.RequeueAfter > 0 {
			return storageResult, nil
		}
		if cacheResult.RequeueAfter > 0 {
			return cacheResult, nil
		}
		if mailResult.RequeueAfter > 0 {
			return mailResult, nil
		}
		return appsResult, nil
	}
	// Infrastructure is ready. Requeue for mail if it is still converging
	// (e.g. waiting for an external SMTP source secret to appear).
	if mailResult.RequeueAfter > 0 {
		logger.Info("tenant ready; mail still converging", "tenant", tenant.Name)
		return mailResult, nil
	}
	logger.Info("tenant reconciled successfully", "tenant", tenant.Name)
	return ctrl.Result{}, nil
}

// reconcileDelete handles Tenant deletion based on deletionPolicy.
func (r *TenantReconciler) reconcileDelete(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	nsName := tenantNamespaceName(tenant)

	// Clean up identity resources before removing the namespace.
	if err := r.deleteIdentity(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up LDAP resources before removing the namespace.
	if err := r.deleteLDAP(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up database resources before removing the namespace.
	if err := r.deleteDatabase(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up MariaDB resources before removing the namespace.
	if err := r.deleteMariaDB(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up storage resources before removing the namespace.
	if err := r.deleteStorage(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up cache resources before removing the namespace.
	if err := r.deleteCache(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up app deployment resources (always, regardless of DeletionPolicy).
	if err := r.deleteAppDeployment(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up Ingress and Certificate resources (ephemeral routing; always deleted).
	if err := r.deleteIngress(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up IntegrationBinding CRs (always deleted regardless of DeletionPolicy).
	if err := r.deleteIntegrationBindings(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up mail resources (Application CRs always; Secrets under DeletionPolicy=Delete).
	if err := r.deleteMail(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	if tenant.Spec.DeletionPolicy == gentianov1alpha1.DeletionPolicyDelete {
		logger.Info("deletionPolicy=Delete: removing tenant namespace", "namespace", nsName)
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns); err == nil {
			if err := r.Delete(ctx, ns); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// DeletionPolicyRetain: keep namespace, only remove orchestrator-owned sub-resources
		logger.Info("deletionPolicy=Retain: preserving tenant namespace", "namespace", nsName)
		if err := r.deleteOwnedResourcesInNamespace(ctx, nsName); err != nil {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(tenant, tenantFinalizer)
	return ctrl.Result{}, r.Update(ctx, tenant)
}

// deleteOwnedResourcesInNamespace removes ResourceQuota, LimitRange, and NetworkPolicy
// owned by the orchestrator from the namespace, leaving customer workloads intact.
func (r *TenantReconciler) deleteOwnedResourcesInNamespace(ctx context.Context, nsName string) error {
	rq := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-quota", Namespace: nsName,
	}}
	if err := r.Delete(ctx, rq); client.IgnoreNotFound(err) != nil {
		return err
	}

	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-limits", Namespace: nsName,
	}}
	if err := r.Delete(ctx, lr); client.IgnoreNotFound(err) != nil {
		return err
	}

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-isolation", Namespace: nsName,
	}}
	if err := r.Delete(ctx, np); client.IgnoreNotFound(err) != nil {
		return err
	}

	return nil
}

// ensureNamespace creates or updates the tenant namespace.
func (r *TenantReconciler) ensureNamespace(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	desired := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
	}

	existing := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Ensure the tenant label is present (idempotent patch)
	if existing.Labels[tenantLabel] != tenant.Name {
		patch := client.MergeFrom(existing.DeepCopy())
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[tenantLabel] = tenant.Name
		existing.Labels[managedByLabel] = managedByValue
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureResourceQuota creates or updates a ResourceQuota in the tenant namespace.
func (r *TenantReconciler) ensureResourceQuota(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	rl := buildResourceList(tenant.Spec.Quotas)
	if len(rl) == 0 {
		return nil
	}

	desired := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-quota",
			Namespace: nsName,
			Labels:    map[string]string{tenantLabel: tenant.Name, managedByLabel: managedByValue},
		},
		Spec: corev1.ResourceQuotaSpec{Hard: rl},
	}

	existing := &corev1.ResourceQuota{}
	err := r.Get(ctx, types.NamespacedName{Name: "tenant-quota", Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if !equality.Semantic.DeepEqual(existing.Spec.Hard, desired.Spec.Hard) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.Hard = desired.Spec.Hard
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureLimitRange creates or updates a LimitRange with sensible per-container defaults.
func (r *TenantReconciler) ensureLimitRange(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	desired := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-limits",
			Namespace: nsName,
			Labels:    map[string]string{tenantLabel: tenant.Name, managedByLabel: managedByValue},
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Default: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					DefaultRequest: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Max: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("8Gi"),
					},
				},
			},
		},
	}

	existing := &corev1.LimitRange{}
	err := r.Get(ctx, types.NamespacedName{Name: "tenant-limits", Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if !equality.Semantic.DeepEqual(existing.Spec.Limits, desired.Spec.Limits) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.Limits = desired.Spec.Limits
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureNetworkPolicy creates or updates the tenant isolation NetworkPolicy.
// - Allows all egress to the platform-kernel namespace and DNS.
// - Denies ingress from other tenant namespaces.
// - Allows ingress from within the same namespace (intra-tenant).
func (r *TenantReconciler) ensureNetworkPolicy(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	protocolTCP := corev1.ProtocolTCP
	protocolUDP := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)

	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-isolation",
			Namespace: nsName,
			Labels:    map[string]string{tenantLabel: tenant.Name, managedByLabel: managedByValue},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // applies to all pods in namespace
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Allow ingress from pods within the same tenant namespace
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{tenantLabel: tenant.Name},
							},
						},
					},
				},
				{
					// Allow ingress from the kernel namespace
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"kubernetes.io/metadata.name": kernelNamespace},
							},
						},
					},
				},
				{
					// Allow ingress from the nginx ingress controller namespace so that
					// the controller can proxy external HTTP/S traffic to tenant services.
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"kubernetes.io/metadata.name": ingressNamespace},
							},
						},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Allow all egress to platform-kernel namespace
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"kubernetes.io/metadata.name": kernelNamespace},
							},
						},
					},
				},
				{
					// Allow egress to the shared infra namespace (MariaDB, Redis, MinIO).
					// See infraNamespace constant — TODO: make configurable per environment.
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"kubernetes.io/metadata.name": infraNamespace},
							},
						},
					},
				},
				{
					// Allow egress within the same tenant namespace
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{tenantLabel: tenant.Name},
							},
						},
					},
				},
				{
					// Allow DNS egress (kube-dns / CoreDNS)
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &protocolUDP, Port: &dnsPort},
						{Protocol: &protocolTCP, Port: &dnsPort},
					},
				},
			},
		},
	}

	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: "tenant-isolation", Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = desired.Spec
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// setCondition upserts a metav1.Condition on the Tenant status.
func (r *TenantReconciler) setCondition(tenant *gentianov1alpha1.Tenant, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range tenant.Status.Conditions {
		if c.Type == condType {
			if c.Status == status && c.Reason == reason {
				return
			}
			tenant.Status.Conditions[i] = metav1.Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: tenant.Generation,
			}
			return
		}
	}
	tenant.Status.Conditions = append(tenant.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: tenant.Generation,
	})
}

// tenantNamespaceName returns the namespace name for the tenant.
// Uses spec.isolation.namespace if set, otherwise "tenant-{name}".
func tenantNamespaceName(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.Namespace != "" {
		return tenant.Spec.Isolation.Namespace
	}
	return fmt.Sprintf("tenant-%s", tenant.Name)
}

// buildResourceList converts TenantQuotas to a corev1.ResourceList.
func buildResourceList(q *gentianov1alpha1.TenantQuotas) corev1.ResourceList {
	if q == nil {
		return nil
	}
	rl := corev1.ResourceList{}
	if q.Storage != nil {
		rl[corev1.ResourceRequestsStorage] = *q.Storage
	}
	if q.CPU != nil {
		rl[corev1.ResourceLimitsCPU] = *q.CPU
	}
	if q.Memory != nil {
		rl[corev1.ResourceLimitsMemory] = *q.Memory
	}
	return rl
}
