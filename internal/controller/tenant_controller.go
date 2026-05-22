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
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	tenantFinalizer = "gentianos.io/tenant-cleanup"
	tenantLabel     = "gentianos.io/tenant"
	managedByLabel  = "app.kubernetes.io/managed-by"
	managedByValue  = "gentian-os"
	kernelNamespace = "platform-kernel"
	// sharedAppsNamespace is the namespace where shared App claims and their
	// Helm releases (e.g. Synapse + Element) are deployed. It mirrors the
	// shared-apps Keycloak realm: one deployment serves all tenants with
	// isolationMode: shared for the same app profile.
	sharedAppsNamespace = "shared-apps"
	// ingressNamespace is the namespace where the nginx ingress controller runs.
	// Pods in this namespace must be allowed ingress to tenant pods so that the
	// controller can proxy external requests to services inside the tenant namespace.
	ingressNamespace        = "ingress"
	conditionNamespaceReady = "NamespaceReady"
)

// infraNamespace and servicesNamespace are read from env vars at process
// startup so that staging/prod deployments can override them without rebuilding
// the operator. Defaults match the dev install layout.
// Inject via INFRA_NAMESPACE / SERVICES_NAMESPACE in the operator Deployment
// (see charts/gentian-os/templates/deployment.yaml).
var (
	infraNamespace    = envOrDefault("INFRA_NAMESPACE", "gentian-infra-dev")
	servicesNamespace = envOrDefault("SERVICES_NAMESPACE", "gentian-dev")
)

// errDeleteJobPending is returned by delete helpers when a cleanup Job has been
// created but has not yet completed. reconcileDelete treats this as a signal to
// requeue rather than a hard error, so the finalizer is only removed once all
// cleanup Jobs have finished.
var errDeleteJobPending = fmt.Errorf("cleanup job not yet complete")

// deleteProvisioningJobs removes completed provisioning Jobs by name, ignoring
// not-found and transient errors. Call this after a cleanup Job completes so
// that the provisioning Jobs are re-created (and the resource re-provisioned)
// on the next tenant deploy.
func (r *TenantReconciler) deleteProvisioningJobs(ctx context.Context, jobNames ...string) {
	prop := metav1.DeletePropagationBackground
	for _, name := range jobNames {
		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, job); err != nil {
			continue
		}
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// xTenantGVK is the GroupVersionKind for the XTenant composite resource managed
// by Crossplane. The TenantReconciler creates one XTenant per Tenant so the
// Crossplane Composition can provision namespace, networking, OpenBao policy,
// and App claims declaratively alongside the imperative Go provisioners.
var xTenantGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "XTenant",
}

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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Seeder derives and persists per-tenant-per-app credentials into OpenBao.
	// May be nil — in which case all reconcilers skip the seeding step and behave
	// exactly as they did before Inc 21a. This keeps existing envtest suites
	// passing without requiring an OpenBao test double.
	Seeder *secrets.Seeder
	// KernelDomain is the cluster-wide platform domain (e.g. `desk.gentian.org`)
	// on which the kernel UIs (Keycloak, Argo CD, Nubus, Intercom) and the
	// `<tenant>.<kernel_domain>` fallback for tenants without a vanity domain
	// are served. Sourced from the KERNEL_DOMAIN env var at startup.
	// See docs/architecture.md §2.5.
	KernelDomain string
	// KernelRealm is the name of the shared Keycloak realm that holds all
	// platform users (synced via Nubus LDAP). Defaults to "kernel".
	// Sourced from the KERNEL_REALM env var at startup.
	KernelRealm string
	// LDAPServer is the LDAP connection URL including scheme and port
	// (e.g. ldap://nubus-ldap.gentian-dev.svc.cluster.local:389).
	// When empty, LDAP federation is not configured for tenant Keycloak realms.
	// Sourced from the LDAP_SERVER env var at startup.
	LDAPServer string
	// LDAPBase is the LDAP base DN (e.g. dc=swp-ldap,dc=internal).
	// Used to construct per-tenant bind DNs and users DNs for LDAP federation.
	// Sourced from the LDAP_BASE env var at startup.
	LDAPBase string
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

	// mapAppProfileToTenants maps an AppProfile change to reconcile requests for
	// every Tenant that references the profile in its spec.apps list.
	mapAppProfileToTenants := func(ctx context.Context, obj client.Object) []reconcile.Request {
		profileName := obj.GetName()
		tenantList := &gentianov1alpha1.TenantList{}
		if err := mgr.GetClient().List(ctx, tenantList); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for _, t := range tenantList.Items {
			for _, app := range t.Spec.Apps {
				if app.Profile == profileName {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{Name: t.Name},
					})
					break
				}
			}
		}
		return requests
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
		Watches(
			&gentianov1alpha1.AppProfile{},
			handler.EnqueueRequestsFromMapFunc(mapAppProfileToTenants),
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

	// Preflight gate: a tenant may only proceed when all requested AppProfiles
	// exist.
	missingProfiles, err := r.validateTenantPrerequisites(ctx, tenant)
	if err != nil {
		reconcileErrors.WithLabelValues("tenant").Inc()
		return ctrl.Result{}, err
	}
	if len(missingProfiles) > 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "ProfileNotFound",
			fmt.Sprintf("AppProfile(s) not found: %s", strings.Join(missingProfiles, ", ")))
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse, "PrerequisitesFailed",
			"Identity provisioning blocked because one or more requested AppProfiles are missing")
		tenant.Status.Phase = gentianov1alpha1.TenantPhaseDegraded
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, nil
	}

	nsName := tenantNamespaceName(tenant)
	logger.Info("reconciling tenant", "tenant", tenant.Name, "namespace", nsName)

	// Phase 3: Ensure the XTenant composite exists so the Crossplane Composition
	// can provision the tenant namespace, networking, OpenBao policy, and App
	// claims declaratively. This runs alongside the existing imperative steps
	// below; the two paths are idempotent and will gradually converge in Phase 3b
	// as imperative steps are migrated into the Composition.
	if err := r.ensureTenantXR(ctx, tenant); err != nil {
		logger.Error(err, "ensure XTenant composite (non-blocking, will retry)")
		// Non-fatal: log the error and continue with imperative provisioning.
	}

	// 1. Namespace
	if err := r.ensureNamespace(ctx, tenant, nsName); err != nil {
		r.setCondition(tenant, conditionNamespaceReady, metav1.ConditionFalse, "EnsureFailed", err.Error())
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 1b. Replicate kernel registry-credentials Secret into the tenant namespace
	// so charts pulling from private registries (e.g. registry.opencode.de) work
	// without per-tenant ExternalSecret boilerplate.
	if err := r.ensureRegistryCredentials(ctx, tenant, nsName); err != nil {
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

	// 12. Ingress (per-app Ingress + TLS — kernel-wildcard fallback or per-host HTTP-01 for vanity domains; see ingress_reconciler.go)
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

	// 14. Mail kernel extension (shared Postfix + Dovecot registration, or external/relay/disabled)
	mailResult, err := r.ensureMail(ctx, tenant)
	if err != nil {
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 14b. Office kernel extension (shared Collabora WOPI service).
	officeResult, err := r.ensureOffice(ctx, tenant)
	if err != nil {
		_ = r.Status().Update(ctx, tenant)
		return ctrl.Result{}, err
	}

	// 14c. Nextcloud kernel file-storage service — provision a per-tenant group in
	// the shared Nextcloud instance for every tenant regardless of installed apps.
	// Non-blocking: errors are returned so the reconciler retries, but this step
	// does not affect the provisioning flag or Phase=Ready (same model as mail).
	if err := r.ensureNextcloudGroup(ctx, tenant); err != nil {
		logger.Error(err, "ensure Nextcloud group (non-blocking, will retry)")
	}

	// 14c. LDAP base infrastructure — provision OU, delegated-admin policy, and
	// admin user for tenants with no LDAP-requiring apps. For tenants WITH LDAP
	// apps this is a no-op (ensureLDAP already handles all steps). Non-blocking:
	// errors are logged and retried without blocking Phase=Ready.
	if err := r.ensureLDAPBase(ctx, tenant); err != nil {
		logger.Error(err, "ensure LDAP base (non-blocking, will retry)")
	}

	// 15. Update status
	r.setCondition(tenant, conditionNamespaceReady, metav1.ConditionTrue, "Provisioned", "Tenant namespace is ready")
	tenant.Status.Namespace = nsName
	tenant.Status.AppCount = len(tenant.Spec.Apps)
	tenant.Status.ReadyApps = len(tenant.Status.ProvisionedApps)
	provisioning := identityResult.RequeueAfter > 0 || ldapResult.RequeueAfter > 0 ||
		databaseResult.RequeueAfter > 0 || mariadbResult.RequeueAfter > 0 ||
		storageResult.RequeueAfter > 0 || cacheResult.RequeueAfter > 0
	// Note: mailResult and officeResult are intentionally excluded from the
	// provisioning flag. Mail and office are kernel extensions and do not
	// block Phase=Ready. Their own conditions track state independently.
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
		if officeResult.RequeueAfter > 0 {
			return officeResult, nil
		}
		return appsResult, nil
	}
	// Infrastructure is ready. Requeue for mail or office if still converging.
	if mailResult.RequeueAfter > 0 {
		logger.Info("tenant ready; mail still converging", "tenant", tenant.Name)
		return mailResult, nil
	}
	if officeResult.RequeueAfter > 0 {
		logger.Info("tenant ready; office still converging", "tenant", tenant.Name)
		return officeResult, nil
	}
	logger.Info("tenant reconciled successfully", "tenant", tenant.Name)
	return ctrl.Result{}, nil
}

// validateTenantPrerequisites checks that all requested AppProfiles exist.
func (r *TenantReconciler) validateTenantPrerequisites(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	missingMap := map[string]struct{}{}

	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				missingMap[app.Profile] = struct{}{}
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
	}

	missing := make([]string, 0, len(missingMap))
	for name := range missingMap {
		missing = append(missing, name)
	}

	return missing, nil
}

// reconcileDelete handles Tenant deletion based on deletionPolicy.
func (r *TenantReconciler) reconcileDelete(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	nsName := tenantNamespaceName(tenant)

	// awaitJob wraps each delete helper: if the helper returns errDeleteJobPending
	// the reconciler requeues rather than treating it as a hard error.
	awaitJob := func(err error) (bool, ctrl.Result, error) {
		if err == nil {
			return false, ctrl.Result{}, nil
		}
		if err == errDeleteJobPending {
			return true, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return true, ctrl.Result{}, err
	}

	// Clean up identity resources before removing the namespace.
	if requeue, res, err := awaitJob(r.deleteIdentity(ctx, tenant)); requeue {
		return res, err
	}

	// Clean up LDAP resources before removing the namespace.
	if requeue, res, err := awaitJob(r.deleteLDAP(ctx, tenant)); requeue {
		return res, err
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
	if requeue, res, err := awaitJob(r.deleteStorage(ctx, tenant)); requeue {
		return res, err
	}

	// Clean up cache resources before removing the namespace.
	if requeue, res, err := awaitJob(r.deleteCache(ctx, tenant)); requeue {
		return res, err
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

	// Phase 3: Delete the XTenant composite so Crossplane cascades deletion of
	// the Composition-managed resources (Namespace, NetworkPolicy, OpenBao policy,
	// App claims). "Not found" is treated as already deleted.
	if err := r.deleteXTenant(ctx, tenant); err != nil {
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

	// If the namespace is still being terminated (e.g. from a prior undeploy via
	// Crossplane cascade), block until it is fully gone. Returning an error here
	// causes the reconciler to requeue, which is preferable to the opaque
	// "unable to create new content in namespace … because it is being terminated"
	// errors that surface later when we try to create Secrets inside it.
	if existing.DeletionTimestamp != nil {
		return fmt.Errorf("namespace %q is still terminating; will retry", nsName)
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

// ensureRegistryCredentials replicates the kernel-managed `registry-credentials`
// dockerconfigjson Secret from the services namespace (where ESO materialises it
// from OpenBao) into the tenant namespace, so that tenant pods using charts
// pinned to private registries (e.g. registry.opencode.de for the
// opendesk-element-web image) can pull their images.
//
// This is the persistent, DRY fix: every tenant namespace gets pull access
// automatically, without each AppProfile or each tenant having to declare a
// per-tenant ExternalSecret.
//
// The replicated Secret is owned by the operator (overwritten on drift) and
// labelled with the tenant so it is cleaned up with the namespace.
func (r *TenantReconciler) ensureRegistryCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	const secretName = "registry-credentials"

	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: servicesNamespace}, source); err != nil {
		if errors.IsNotFound(err) {
			// Soft-fail: if the kernel secret does not exist yet, skip without
			// error. The operator will retry on the next reconcile.
			return nil
		}
		return fmt.Errorf("failed to read source registry-credentials in %s: %w", servicesNamespace, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Type: source.Type,
		Data: source.Data,
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Data, desired.Data) || existing.Type != desired.Type {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Type = desired.Type
		existing.Data = desired.Data
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
	apiServerPort := intstr.FromInt32(443)

	// Resolve the Kubernetes API server ClusterIP from the well-known "kubernetes"
	// service in the default namespace. We use an ipBlock rule (not a namespace
	// selector) because the ClusterIP is a virtual IP — it is not backed by a pod
	// and therefore cannot be matched by a namespaceSelector or podSelector.
	kubeAPISvc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: "kubernetes", Namespace: "default"}, kubeAPISvc); err != nil {
		return fmt.Errorf("failed to look up kubernetes ClusterIP: %w", err)
	}
	kubeAPIServerCIDR := kubeAPISvc.Spec.ClusterIP + "/32"

	// Also resolve the actual API server Endpoints (the real host IP + port that
	// kube-proxy DNATs ClusterIP traffic to). Calico in iptables mode enforces
	// egress NetworkPolicy AFTER kube-proxy performs DNAT, so by the time the
	// packet reaches the Calico filter chain the destination is already the
	// endpoint IP:port, not the ClusterIP:443. We need rules for both so the
	// policy works regardless of where in the iptables pipeline Calico hooks.
	kubeAPIEndpts := &discoveryv1.EndpointSlice{}
	if err := r.Get(ctx, types.NamespacedName{Name: "kubernetes", Namespace: "default"}, kubeAPIEndpts); err != nil {
		return fmt.Errorf("failed to look up kubernetes endpoints: %w", err)
	}

	// Start with the static egress rules.
	egressRules := []networkingv1.NetworkPolicyEgressRule{
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
			// Allow egress to the services namespace (Nubus/Keycloak OIDC, UDM
			// provisioning API for ox-connector). See servicesNamespace constant.
			To: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": servicesNamespace},
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
			// Allow egress to the nginx ingress controller namespace so that
			// tenant pods can reach services via their ingress URLs internally.
			To: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": ingressNamespace},
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
		{
			// Pre-DNAT rule: allow egress to the Kubernetes API ClusterIP:443.
			// Covers CNIs that evaluate NetworkPolicy before kube-proxy DNAT.
			To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: kubeAPIServerCIDR}},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &protocolTCP, Port: &apiServerPort},
			},
		},
	}

	// Post-DNAT rules: one rule per API server endpoint address×port.
	// Calico in iptables mode evaluates egress policy after kube-proxy DNAT, so
	// the destination seen by Calico is the real endpoint IP:port, not the ClusterIP.
	// EndpointSlice ports are at the slice level (shared by all endpoints).
	for _, ep := range kubeAPIEndpts.Endpoints {
		for _, addr := range ep.Addresses {
			for _, port := range kubeAPIEndpts.Ports {
				if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP {
					continue
				}
				if port.Port == nil {
					continue
				}
				endpointPort := intstr.FromInt32(*port.Port)
				egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
					To: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: addr + "/32"}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &protocolTCP, Port: &endpointPort},
					},
				})
			}
		}
	}

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
					// Allow ingress from the shared infra namespace (MariaDB, Redis, MinIO)
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"kubernetes.io/metadata.name": infraNamespace},
							},
						},
					},
				},
				{
					// Allow ingress from the services namespace (Nextcloud, Nubus/Keycloak, etc.)
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"kubernetes.io/metadata.name": servicesNamespace},
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
			Egress: egressRules,
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

// ── Phase 3: XTenant helpers ─────────────────────────────────────────────────

// ensureTenantXR creates or updates the XTenant composite resource that drives
// the Crossplane Composition for this tenant. The Composition provisions the
// tenant namespace, networking, OpenBao policy, and App claims declaratively.
func (r *TenantReconciler) ensureTenantXR(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, xr)
	if errors.IsNotFound(err) {
		return r.Create(ctx, buildXTenant(tenant, r.KernelDomain))
	}
	if err != nil {
		return err
	}
	// Patch only the user-controlled spec fields so that Crossplane-managed
	// fields (compositionRef, resourceRefs, managementPolicies, etc.) are not
	// overwritten. We build the desired state, then merge-patch just the fields
	// that differ.
	desired := buildXTenant(tenant, r.KernelDomain)
	patch := client.MergeFrom(xr.DeepCopy())
	desiredSpec, _ := desired.Object["spec"].(map[string]interface{})
	if specMap, ok := xr.Object["spec"].(map[string]interface{}); ok {
		for k, v := range desiredSpec {
			specMap[k] = v
		}
	} else {
		xr.Object["spec"] = desiredSpec
	}
	return r.Patch(ctx, xr, patch)
}

// deleteXTenant deletes the XTenant composite. Crossplane cascades deletion to
// all composed resources (Namespace, NetworkPolicy, OpenBao policy, App claims).
// "Not found" is treated as already deleted (idempotent).
func (r *TenantReconciler) deleteXTenant(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	xr.SetName(tenant.Name)
	return client.IgnoreNotFound(r.Delete(ctx, xr))
}

// buildXTenant constructs an XTenant composite object from a Tenant's spec.
// The XTenant is cluster-scoped; its name matches the Tenant name.
func buildXTenant(tenant *gentianov1alpha1.Tenant, kernelDomain string) *unstructured.Unstructured {
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	xr.SetName(tenant.Name)
	xr.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})

	spec := map[string]interface{}{
		"displayName":  tenant.Spec.DisplayName,
		"adminEmail":   tenant.Spec.AdminEmail,
		"kernelDomain": kernelDomain,
	}
	if tenant.Spec.Domain != "" {
		spec["domain"] = tenant.Spec.Domain
	}
	if tenant.Spec.DeletionPolicy != "" {
		spec["deletionPolicy"] = string(tenant.Spec.DeletionPolicy)
	}

	if tenant.Spec.Isolation != nil {
		iso := map[string]interface{}{}
		if tenant.Spec.Isolation.Mode != "" {
			iso["mode"] = string(tenant.Spec.Isolation.Mode)
		}
		if tenant.Spec.Isolation.Namespace != "" {
			iso["namespace"] = tenant.Spec.Isolation.Namespace
		}
		if tenant.Spec.Isolation.LDAPOu != "" {
			iso["ldapOU"] = tenant.Spec.Isolation.LDAPOu
		}
		if tenant.Spec.Isolation.KeycloakRealm != "" {
			iso["keycloakRealm"] = tenant.Spec.Isolation.KeycloakRealm
		}
		if tenant.Spec.Isolation.DatabasePrefix != "" {
			iso["databasePrefix"] = tenant.Spec.Isolation.DatabasePrefix
		}
		if tenant.Spec.Isolation.S3Prefix != "" {
			iso["s3Prefix"] = tenant.Spec.Isolation.S3Prefix
		}
		if len(iso) > 0 {
			spec["isolation"] = iso
		}
	}

	apps := make([]interface{}, 0, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		entry := map[string]interface{}{"profile": app.Profile}
		if app.Config != nil {
			cfg := map[string]interface{}{}
			if app.Config.Replicas != nil {
				cfg["replicas"] = int64(*app.Config.Replicas)
			}
			if len(cfg) > 0 {
				entry["config"] = cfg
			}
		}
		apps = append(apps, entry)
	}
	spec["apps"] = apps

	_ = unstructured.SetNestedField(xr.Object, spec, "spec")
	return xr
}
